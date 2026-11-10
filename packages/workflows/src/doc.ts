import type { JsonValue } from '@earendil-works/chord';
import { defineDoc } from '@earendil-works/pi-durable';

// Documents are strict JSON, so absent values are null rather than optional fields.
export type StepRecord = {
  name: string;
  kind: 'code' | 'model' | 'judged' | 'decision';
  status: 'pending' | 'running' | 'waiting' | 'done' | 'failed' | 'aborted';
  startedAt: number | null;
  durationMs: number | null;
  cost: number;
  /** Set by steps that work through units (pages); absent on records written before it existed. */
  progress?: { done: number; total: number } | null;
  error: string | null;
};

/** `kept` holds the results of the steps before a failed one, so a resume starts from them; absent on records written before it existed. */
type WorkflowState = { name: string | null; task: string | null; request: string | null; steps: StepRecord[]; kept?: Record<string, JsonValue> | null };

/** What the session view renders (WFL-4): each step's status, duration and cost. */
export const WorkflowDoc = defineDoc<WorkflowState>({
  kind: 'quack.workflow',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ name: null, task: null, request: null, steps: [], kept: null }),
  checkpointWhen: () => false,
});

