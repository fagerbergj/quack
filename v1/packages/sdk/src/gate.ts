import type { JsonValue } from '@earendil-works/chord';
import type { Context } from '@earendil-works/chord/context';
import type { TSchema } from '@earendil-works/pi-ai';
import { whileWaiting } from './signals.ts';
import { defineDoc, type Conversation, type ToolExecutionResult, type ToolRegistration } from '@earendil-works/pi-durable';

// Documents are strict JSON, so absent values are null rather than optional fields.
export type Verdict = { decision: 'approve' | 'reject'; reason: string | null; by: string; at: number };

export type Approval = {
  tool: string;
  args: JsonValue;
  requestedAt: number;
  verdict: Verdict | null;
  // Set once execution starts, so a crash mid-execution is reported instead of re-running a side effect.
  executing: boolean;
  done: boolean;
};

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

function errorResult(text: string): ToolExecutionResult {
  return { output: [{ type: 'text', text }], isError: true };
}

// Waits until the approval has a verdict, or rejects when the run is aborted.
async function waitForVerdict(watch: { value: Readonly<ApprovalsDoc> | null; start: Function; stop: () => Promise<unknown> }, id: string, ctx: Context) {
  try {
    return await new Promise<Verdict>((resolve, reject) => {
      const found = () => watch.value?.items[id]?.verdict;
      if (found()) return resolve(plain(found()!));
      ctx.abortSignal?.addEventListener('abort', () => reject(ctx.abortSignal!.reason), { once: true });
      watch.start(async () => { if (found()) resolve(plain(found()!)); });
    });
  } finally {
    await watch.stop();
  }
}

// A gated tool waits durably for a verdict before it runs (GATE-5..8). Replay-safe: a rerun after a crash finds its approval by task id.
export function gated<P extends TSchema>(tool: ToolRegistration<P>): ToolRegistration<P> {
  return {
    ...tool,
    replay: 'safe',
    async execute(args, api, ctx) {
      const id = String(api.taskId);
      const item = await api.commit(async (tx) => {
        const doc = await tx.doc(Approvals, api.conversationId);
        doc.items[id] ??= { tool: tool.name, args: plain(args) as JsonValue, requestedAt: api.now(), verdict: null, executing: false, done: false };
        return plain(doc.items[id]);
      }, ctx);
      if (item.executing && !item.done) return errorResult(`${tool.name} was interrupted after approval and was not re-run.`);

      const watch = await api.watchDoc(Approvals, api.conversationId, ctx);
      if (!watch) return errorResult('approval state is unavailable');
      const verdict = item.verdict ?? (await whileWaiting(() => waitForVerdict(watch as never, id, ctx)));
      if (verdict.decision === 'reject') return errorResult(`Rejected by ${verdict.by}${verdict.reason ? `: ${verdict.reason}` : ''}`);

      await api.commit(async (tx) => { (await tx.doc(Approvals, api.conversationId)).items[id].executing = true; }, ctx);
      const result = await tool.execute(args, api, ctx);
      await api.commit(async (tx) => { (await tx.doc(Approvals, api.conversationId)).items[id].done = true; }, ctx);
      return result;
    },
  };
}

export async function pendingApprovals(conversation: Conversation, ctx: Context): Promise<Array<Approval & { id: string }>> {
  const doc = await conversation.commit(async (tx) => plain(await tx.doc(Approvals, conversation.id)), ctx);
  return Object.entries(doc.items).filter(([, a]) => !a.verdict).map(([id, a]) => ({ id, ...a }));
}

// Records a verdict; returns false when the approval is unknown or already decided.
export async function resolveApproval(conversation: Conversation, id: string, verdict: { decision: Verdict['decision']; reason?: string; by: string }, ctx: Context): Promise<boolean> {
  return conversation.commit(async (tx) => {
    const doc = await tx.doc(Approvals, conversation.id);
    const item = doc.items[id];
    if (!item || item.verdict) return false;
    item.verdict = { decision: verdict.decision, reason: verdict.reason ?? null, by: verdict.by, at: Date.now() };
    return true;
  }, ctx);
}
