import type { Context } from '@earendil-works/chord/context';
import { defineDoc, type Conversation, type DocumentWatch } from '@earendil-works/pi-durable';

// Documents are strict JSON, so absent values are null rather than optional fields.
export type StepRecord = {
  name: string;
  kind: 'code' | 'model' | 'decision' | 'gated';
  status: 'pending' | 'running' | 'waiting' | 'done' | 'failed';
  startedAt: number | null;
  durationMs: number | null;
  cost: number;
  error: string | null;
  /** A decision step's open question; the answer is written by `answerDecision`. */
  ask: { question: string; options: string[] } | null;
  answer: { choice: string; by: string } | null;
};

type WorkflowState = { name: string | null; task: string | null; request: string | null; steps: StepRecord[] };

/** What the session view renders (WFL-4): each step's status, duration and cost. */
export const WorkflowDoc = defineDoc<WorkflowState>({
  kind: 'quack.workflow',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ name: null, task: null, request: null, steps: [] }),
  checkpointWhen: () => false,
});

const plain = <T>(value: T): T => JSON.parse(JSON.stringify(value));

/** Resolves once `pick` finds a value in the watched document, or rejects when the run is aborted. */
export async function watchUntil<V>(watch: DocumentWatch<WorkflowState>, pick: (doc: Readonly<WorkflowState>) => V | undefined | null, signal?: AbortSignal): Promise<V> {
  try {
    return await new Promise<V>((resolve, reject) => {
      const found = () => (watch.value ? pick(watch.value) : undefined) ?? undefined;
      const now = found();
      if (now !== undefined) return resolve(plain(now));
      signal?.addEventListener('abort', () => reject(signal.reason), { once: true });
      watch.start(async () => { const v = found(); if (v !== undefined) resolve(plain(v)); });
    });
  } finally {
    await watch.stop();
  }
}

/** Records a person's answer to a waiting decision step; false when the step is unknown, not asking, or already answered. */
export async function answerDecision(conversation: Conversation, step: string, choice: string, by: string, ctx: Context): Promise<boolean> {
  return conversation.commit(async (tx) => {
    const s = (await tx.doc(WorkflowDoc, conversation.id)).steps.find((r) => r.name === step);
    if (!s?.ask || s.answer || !s.ask.options.includes(choice)) return false;
    s.answer = { choice, by };
    return true;
  }, ctx);
}
