import { isDeepStrictEqual } from 'node:util';
import type { JsonValue } from '@earendil-works/chord';
import type { Context } from '@earendil-works/chord/context';
import { principalName, type Principal } from './principal.ts';
import { whileWaiting } from './signals.ts';
import { approvalOpenedData, eventBytes, EVENT_MAX_BYTES } from './channel.ts';
import { runApprovalGates, type ApprovalChecks } from './gates.ts';
import { defineDoc, InboxDoc, type Conversation, type ConversationId, type Tx, type ToolExecutionApi, type ToolExecutionResult, type ToolRegistration } from '@earendil-works/pi-durable';

// Documents are strict JSON, so absent values are null rather than optional fields.
// `by` is a bare string in verdicts recorded before principals were typed.
export type Verdict = { decision: 'approve' | 'reject'; reason: string | null; by: Principal | string; at: number };

export type Approval = {
  tool: string;
  args: JsonValue;
  requestedAt: number;
  verdict: Verdict | null;
  // Set once execution starts, so a crash mid-execution is reported instead of re-running a side effect.
  executing: boolean;
  done: boolean;
  // Absent on rows written before withdrawal existed, which read as null.
  withdrawn?: Withdrawal | null;
  /** The sealed artifact version the approval decides on; a person's approve of one runs the approval.approving gates before it is recorded. */
  subject?: { kind: string; version: number } | null;
  /** A person's approve the gates are still checking; it becomes the verdict once they pass. */
  approving?: Verdict | null;
  /** What the gates found on the last approve; failed ones leave the approval open, and the next approve is recorded over their findings.
   * Undecided ones (a gate could not run its checks) are run again by the next approve, unless it gives a reason, which records it over them. */
  checks?: ApprovalChecks | null;
};

/** Why the call that asked can no longer act on its approval; core writes task_ended, arguments_changed and steered, an extension superseded. */
export type Withdrawal = { at: number; reason: 'task_ended' | 'superseded' | 'arguments_changed' | 'steered'; supersededBy: string | null };

type ApprovalsDoc = { items: Record<string, Approval> };

export const Approvals = defineDoc<ApprovalsDoc>({
  kind: 'quack.approvals',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ items: {} }),
  checkpointWhen: () => false,
});

const plain = <T>(value: T): T => JSON.parse(JSON.stringify(value));

/** Whether the asking call could still act on the approval: a rejection never acts, and one executing already has. */
export const actionable = (a: Approval) => !a.withdrawn && !a.executing && a.verdict?.decision !== 'reject';

/** Marks each approval `pick` names that its call could still act on as withdrawn, and returns their ids; the caller commits `doc`. */
export function withdrawApprovals(doc: ApprovalsDoc, pick: (id: string, a: Approval) => boolean, w: Withdrawal): string[] {
  const ids = Object.keys(doc.items).filter((id) => actionable(doc.items[id]) && pick(id, doc.items[id]));
  for (const id of ids) doc.items[id].withdrawn = { ...w };
  // A withdrawn approval's held approve will never be checked, so none is left to read as "Checking".
  for (const id of ids) if (doc.items[id].approving) doc.items[id].approving = null;
  return ids;
}

/** The ids of queued follow-ups a named principal sent: pi-durable's inbox item does not say who sent it. */
const PersonInputs = defineDoc<{ ids: string[] }>({
  kind: 'quack.person-inputs', version: 1, scope: 'conversation', history: 'latest', fork: 'initial', initial: () => ({ ids: [] }), checkpointWhen: () => false,
});

/** A queued steer, or a follow-up a named principal sent, withdraws each approval `pick` names that still waits on a verdict; that
 * person's follow-ups turn into steers so the run reads them at its next tool boundary. `person` is an input admitted in this commit. */
export async function withdrawForMessages(tx: Tx, conversationId: ConversationId, doc: ApprovalsDoc, at: number, o: { pick?: (id: string) => boolean; person?: string } = {}): Promise<string[]> {
  const inbox = await tx.doc(InboxDoc, conversationId);
  const people = await tx.doc(PersonInputs, conversationId);
  const queued = new Set(inbox.items.map((i) => String(i.id)));
  const ids = [...people.ids, ...(o.person ? [o.person] : [])].filter((id) => queued.has(id));
  if (!isDeepStrictEqual(ids, people.ids)) people.ids = ids;
  if (!ids.length && !inbox.items.some((i) => i.mode === 'steer')) return [];
  const gone = withdrawApprovals(doc, (id, a) => !a.verdict && (o.pick?.(id) ?? true), { at, reason: 'steered', supersededBy: null });
  // shortcut: pi-durable promotes a follow-up only by abort and resubmit (two commits, a new id), so this edits pi.inbox; use an in-commit API once it has one.
  if (gone.length) for (const i of inbox.items) if (i.mode === 'followUp' && ids.includes(String(i.id))) i.mode = 'steer';
  return gone;
}

const WHY: Record<Withdrawal['reason'], string> = { task_ended: "the call's task ended without acting on it", superseded: 'it was superseded by a newer request', arguments_changed: "the call's arguments changed", steered: "a person's message reached the run while it waited" };

/** What a late answer to a withdrawn approval is told; without a reason, the session's is unknown. */
export const withdrawnText = (reason?: Withdrawal['reason']) => `the request was withdrawn: ${reason ? WHY[reason] : 'the session is no longer waiting for it'}`;

function notRun(tool: string, w: Withdrawal): ToolExecutionResult {
  const again = ({ arguments_changed: ` Call ${tool} again to ask about these arguments.`, steered: ` Answer the message, then call ${tool} again to ask.` } as Partial<Record<Withdrawal['reason'], string>>)[w.reason] ?? '';
  return errorResult(`${tool} was not run: ${withdrawnText(w.reason)}${w.supersededBy ? ` (${w.supersededBy})` : ''}.${again}`);
}

function errorResult(text: string): ToolExecutionResult {
  return { output: [{ type: 'text', text }], isError: true };
}

// Waits until the approval has a verdict or is withdrawn, or rejects when the run is aborted.
async function waitForAnswer(watch: { value: Readonly<ApprovalsDoc> | null; start: Function; stop: () => Promise<unknown> }, id: string, ctx: Context) {
  try {
    return await new Promise<Approval>((resolve, reject) => {
      const found = () => { const a = watch.value?.items[id]; return a?.verdict || a?.withdrawn || a?.approving ? a : undefined; };
      if (found()) return resolve(plain(found()!));
      if (ctx.abortSignal?.aborted) return reject(ctx.abortSignal.reason);
      ctx.abortSignal?.addEventListener('abort', () => reject(ctx.abortSignal!.reason), { once: true });
      watch.start(async () => { if (found()) resolve(plain(found()!)); });
    });
  } finally {
    await watch.stop();
  }
}

// The gates run on a person's approve; a pass records it as the verdict, a failure clears it and keeps the approval open with their findings.
async function checkApprove(api: ToolExecutionApi, id: string, asked: Approval, ctx: Context): Promise<Approval> {
  // A chain that threw ran no checks: it is recorded as undecided, so the approval reopens and a plain approve retries. A stopped task
  // can commit nothing more; the session's Stop withdraws its approvals, clearing the held approve.
  const checks = await runApprovalGates(api, id, asked, ctx).catch((e: unknown): ApprovalChecks => {
    ctx.abortSignal?.throwIfAborted();
    return { at: api.now(), passed: false, decided: false, feedback: [`The checks did not finish: ${e instanceof Error ? e.message : String(e)}`], detail: null };
  });
  return api.commit(async (tx) => {
    const row = (await tx.doc(Approvals, api.conversationId)).items[id];
    if (row.verdict || row.withdrawn) return plain(row);
    row.checks = plain(checks);
    row.verdict = checks.passed ? row.approving! : null;
    row.approving = null;
    return plain(row);
  }, ctx);
}

// Waits until the approval is decided or withdrawn, checking each approve on the way; undefined when approval state cannot be watched.
async function decided(api: ToolExecutionApi, id: string, item: Approval, ctx: Context): Promise<Approval | undefined> {
  let a = item;
  while (!a.verdict && !a.withdrawn) {
    if (!a.approving) {
      const watch = await api.watchDoc(Approvals, api.conversationId, ctx);
      if (!watch) return undefined;
      a = await whileWaiting(() => waitForAnswer(watch as never, id, ctx));
    }
    if (a.approving && !a.verdict && !a.withdrawn) a = await checkApprove(api, id, a, ctx);
  }
  return a;
}

/** `subject` names the sealed artifact version an approval of these arguments decides on, so the approval.approving gates check it. */
export type GatedOptions = { subject?: (args: JsonValue) => { kind: string; version: number } | undefined };

// A gated tool waits durably for a verdict before it runs (GATE-5..8). Replay-safe: a rerun after a crash finds its approval by task id,
// unless it came back with other arguments: a verdict on the first ones must not run these.
export function gated<P extends object>(tool: ToolRegistration<P>, opts: GatedOptions = {}): ToolRegistration<P> {
  return {
    ...tool,
    replay: 'safe',
    async execute(args, api, ctx) {
      const id = String(api.taskId);
      // An approval the lifecycle channel could not carry is refused here, never truncated: the agent sees why.
      const size = eventBytes(`ask:open:${id}`, 'ask.opened', approvalOpenedData(id, tool.name, plain(args) as JsonValue, api.now()));
      if (size > EVENT_MAX_BYTES) return errorResult(`${tool.name} was not run: its arguments are ${size} bytes as an approval, over the ${EVENT_MAX_BYTES}-byte cap; send less.`);
      const item = await api.commit(async (tx) => {
        const conversation = await tx.conversation(api.conversationId);
        const doc = await tx.doc(Approvals, api.conversationId);
        const fresh = plain(args) as JsonValue;
        withdrawApprovals(doc, (k, a) => k === id && !isDeepStrictEqual(a.args, fresh), { at: api.now(), reason: 'arguments_changed', supersededBy: null });
        const subject = opts.subject?.(fresh);
        doc.items[id] ??= { tool: tool.name, args: fresh, requestedAt: api.now(), verdict: null, executing: false, done: false, ...(subject && { subject }) };
        // A message queued before the row opened (or before a restart reran this call) withdraws it at once; a child's rows are left alone.
        if (conversation && !conversation.owner && !conversation.parent) await withdrawForMessages(tx, api.conversationId, doc, api.now(), { pick: (k) => k === id });
        return plain(doc.items[id]);
      }, ctx);
      if (item.executing && !item.done) return errorResult(`${tool.name} was interrupted after approval and was not re-run.`);

      const answered = await decided(api, id, item, ctx);
      if (!answered) return errorResult('approval state is unavailable');
      if (answered.withdrawn) return notRun(tool.name, answered.withdrawn);
      const verdict = answered.verdict!;
      if (verdict.decision === 'reject') return errorResult(`Rejected by ${principalName(verdict.by)}${verdict.reason ? `: ${verdict.reason}` : ''}`);

      await api.commit(async (tx) => { (await tx.doc(Approvals, api.conversationId)).items[id].executing = true; }, ctx);
      const result = await tool.execute(args, api, ctx);
      await api.commit(async (tx) => { (await tx.doc(Approvals, api.conversationId)).items[id].done = true; }, ctx);
      return result;
    },
  };
}

/** The approvals waiting on a person; one whose approve the gates are checking waits on them, not on a person. */
export async function pendingApprovals(conversation: Conversation, ctx: Context): Promise<Array<Approval & { id: string }>> {
  const doc = await conversation.commit(async (tx) => plain(await tx.doc(Approvals, conversation.id)), ctx);
  return Object.entries(doc.items).filter(([, a]) => !a.verdict && !a.withdrawn && !a.approving).map(([id, a]) => ({ id, ...a }));
}

// Records a verdict; returns false when the approval is unknown, already decided or withdrawn, or an approve of it is being checked.
// An approve of a subject waits for the gates, unless they already failed it, or could not run and the person says why they approve anyway.
export async function resolveApproval(conversation: Conversation, id: string, verdict: { decision: Verdict['decision']; reason?: string; by: Principal }, ctx: Context): Promise<boolean> {
  return conversation.commit(async (tx) => {
    const doc = await tx.doc(Approvals, conversation.id);
    const item = doc.items[id];
    if (!item || item.verdict || item.withdrawn) return false;
    const v: Verdict = { decision: verdict.decision, reason: verdict.reason ?? null, by: verdict.by, at: Date.now() };
    const over = item.checks && (item.checks.decided !== false || !!verdict.reason);
    if (verdict.decision === 'reject' || !item.subject || over) item.verdict = v;
    else if (item.approving) return false;
    else item.approving = v;
    return true;
  }, ctx);
}
