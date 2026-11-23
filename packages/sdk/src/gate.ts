import { isDeepStrictEqual } from 'node:util';
import type { Draft, JsonValue } from '@earendil-works/chord';
import type { Context } from '@earendil-works/chord/context';
import { principalName, type Principal } from './principal.ts';
import { whileWaiting } from './signals.ts';
import { approvalOpenedData, eventBytes, EVENT_MAX_BYTES } from './channel.ts';
import { defineDoc, InboxDoc, type Conversation, type InboxState, type ToolExecutionResult, type ToolRegistration } from '@earendil-works/pi-durable';

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
  return ids;
}

/** A person's message queued in the root's inbox withdraws each approval `pick` names that still waits on a verdict, and turns
 * into a steer so the run reads it at its next tool boundary; returns the withdrawn ids. The caller commits both documents. */
export function withdrawForMessages(inbox: Draft<InboxState>, doc: ApprovalsDoc, at: number, pick = (_id: string) => true): string[] {
  if (!inbox.items.some((i) => i.mode !== 'write')) return [];
  const ids = withdrawApprovals(doc, (id, a) => !a.verdict && pick(id), { at, reason: 'steered', supersededBy: null });
  if (ids.length) for (const i of inbox.items) if (i.mode === 'followUp') i.mode = 'steer';
  return ids;
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
      const found = () => { const a = watch.value?.items[id]; return a?.verdict || a?.withdrawn ? a : undefined; };
      if (found()) return resolve(plain(found()!));
      if (ctx.abortSignal?.aborted) return reject(ctx.abortSignal.reason);
      ctx.abortSignal?.addEventListener('abort', () => reject(ctx.abortSignal!.reason), { once: true });
      watch.start(async () => { if (found()) resolve(plain(found()!)); });
    });
  } finally {
    await watch.stop();
  }
}

// A gated tool waits durably for a verdict before it runs (GATE-5..8). Replay-safe: a rerun after a crash finds its approval by task id,
// unless it came back with other arguments: a verdict on the first ones must not run these.
export function gated<P extends object>(tool: ToolRegistration<P>): ToolRegistration<P> {
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
        doc.items[id] ??= { tool: tool.name, args: fresh, requestedAt: api.now(), verdict: null, executing: false, done: false };
        // A message queued before the row opened (or before a restart reran this call) withdraws it at once; a child's rows are left alone.
        if (conversation && !conversation.owner && !conversation.parent) withdrawForMessages(await tx.doc(InboxDoc, api.conversationId), doc, api.now(), (k) => k === id);
        return plain(doc.items[id]);
      }, ctx);
      if (item.executing && !item.done) return errorResult(`${tool.name} was interrupted after approval and was not re-run.`);

      let answered = item;
      if (!item.verdict && !item.withdrawn) {
        const watch = await api.watchDoc(Approvals, api.conversationId, ctx);
        if (!watch) return errorResult('approval state is unavailable');
        answered = await whileWaiting(() => waitForAnswer(watch as never, id, ctx));
      }
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

export async function pendingApprovals(conversation: Conversation, ctx: Context): Promise<Array<Approval & { id: string }>> {
  const doc = await conversation.commit(async (tx) => plain(await tx.doc(Approvals, conversation.id)), ctx);
  return Object.entries(doc.items).filter(([, a]) => !a.verdict && !a.withdrawn).map(([id, a]) => ({ id, ...a }));
}

// Records a verdict; returns false when the approval is unknown, already decided or withdrawn.
export async function resolveApproval(conversation: Conversation, id: string, verdict: { decision: Verdict['decision']; reason?: string; by: Principal }, ctx: Context): Promise<boolean> {
  return conversation.commit(async (tx) => {
    const doc = await tx.doc(Approvals, conversation.id);
    const item = doc.items[id];
    if (!item || item.verdict || item.withdrawn) return false;
    item.verdict = { decision: verdict.decision, reason: verdict.reason ?? null, by: verdict.by, at: Date.now() };
    return true;
  }, ctx);
}
