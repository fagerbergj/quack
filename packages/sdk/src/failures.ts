import { defineDoc } from '@earendil-works/pi-durable';

// One row per step of the latest run; `run` orders runs so a later run's clean row supersedes an earlier failure.
export type FailureRow = { error: string | null; run: number; at: number };

type FailuresDoc = { items: Record<string, FailureRow> };

/** What a run ended on, for the session view and the index; a failed run leaves no transcript, so this is where its error shows. */
export const Failures = defineDoc<FailuresDoc>({
  kind: 'quack.failures',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ items: {} }),
  checkpointWhen: () => false,
});

export const failedRows = (doc: Readonly<FailuresDoc> | null | undefined) =>
  Object.entries(doc?.items ?? {}).filter(([, r]) => r.error !== null).map(([step, r]) => ({ step, error: r.error! }));
