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
  /** Set by steps that work through units (pages); absent on records written before it existed. */
  progress?: { done: number; total: number } | null;
  error: string | null;
  /** A decision step's open question; the answer is written by `answerDecision`. */
  ask: { question: string; options: string[]; evidence?: string | null; at?: number | null } | null;
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

export type AnswerResult = 'ok' | 'not-asking' | 'answered' | 'bad-choice';

/** Records a person's answer to a decision step that is waiting on one. */
export async function answerDecision(conversation: Conversation, step: string, choice: string, by: string, ctx: Context): Promise<AnswerResult> {
  return conversation.commit(async (tx): Promise<AnswerResult> => {
    const s = (await tx.doc(WorkflowDoc, conversation.id)).steps.find((r) => r.name === step);
    if (!s?.ask) return 'not-asking';
    if (s.answer) return 'answered';
    if (!s.ask.options.includes(choice)) return 'bad-choice';
    s.answer = { choice, by };
    return 'ok';
  }, ctx);
}

export type OpenDecision = { step: string; question: string; options: string[]; evidence: string | null; askedAt: number };

/** Decision steps waiting on a person; `ask` stays on a step after it is answered or its run ends, so the status decides. */
export function pendingDecisions(doc: Readonly<WorkflowState> | null | undefined): OpenDecision[] {
  return (doc?.steps ?? []).filter((s) => s.ask && !s.answer && s.status === 'waiting')
    .map((s) => ({ step: s.name, question: s.ask!.question, options: s.ask!.options, evidence: s.ask!.evidence ?? null, askedAt: s.ask!.at ?? 0 }));
}
