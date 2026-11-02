import { defineDoc, type DocumentWatch } from '@earendil-works/pi-durable';

// Documents are strict JSON, so absent values are null rather than optional fields.
export type StepRecord = {
  name: string;
  kind: 'code' | 'model' | 'judged' | 'decision' | 'gated';
  status: 'pending' | 'running' | 'waiting' | 'done' | 'failed' | 'aborted';
  startedAt: number | null;
  durationMs: number | null;
  cost: number;
  /** Set by steps that work through units (pages); absent on records written before it existed. */
  progress?: { done: number; total: number } | null;
  error: string | null;
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
