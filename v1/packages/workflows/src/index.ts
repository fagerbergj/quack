import { BACKGROUND_CONTEXT, type Context } from '@earendil-works/chord/context';
import type { JsonValue } from '@earendil-works/chord';
import { defineExtension, defineTask, type Conversation, type ConversationId, type Extension, type Harness, type RunningTask, type TaskRuntime, type Tx } from '@earendil-works/pi-durable';
import type { Dispatch, DispatchingExtension } from '@quack/session';
import { answerDecision, pendingDecisions, WorkflowDoc, type StepRecord } from './doc.ts';
import { execute, view, type Step, type StepContext } from './steps.ts';

export { answerDecision, pendingDecisions, WorkflowDoc, watchUntil, type AnswerResult, type OpenDecision, type StepRecord } from './doc.ts';
export type { Step, StepContext } from './steps.ts';

type Results = Record<string, JsonValue>;
// The checkpoint is the results so far: committing a step's result and moving to the next phase are one write (WFL-2).
type Checkpoint = { phase: string; results: Results };
type Input = { input: JsonValue };
type Runtime = TaskRuntime<Input, Checkpoint, Results, object>;

export type Workflow = ReturnType<typeof defineWorkflow>;

async function record(tx: Tx, conversationId: ConversationId, name: string) {
  const s = (await tx.doc(WorkflowDoc, conversationId)).steps.find((r) => r.name === name);
  if (!s) throw new Error(`workflow step ${name} has no record`);
  return s;
}

// A tool's api.commit returns the callback's value; a task's commit would read it as the next task state.
const toolCommit = (rt: Runtime) => async <T>(change: (tx: Tx) => T | Promise<T>, ctx: Context): Promise<T> => {
  let out!: T;
  await rt.commit(async (tx) => { out = await change(tx); return undefined; }, ctx);
  return out;
};

async function runStep(steps: Step[], i: number, running: RunningTask<Input, Checkpoint, Results>, rt: Runtime, ctx: Context) {
  const step = steps[i];
  await rt.commit(async (tx) => {
    const s = await record(tx, rt.conversationId, step.name);
    s.status = 'running';
    s.startedAt ??= rt.now();
    return undefined;
  }, ctx);
  const report: StepContext['report'] = (patch) => rt.commit(async (tx) => {
    const s = await record(tx, rt.conversationId, step.name);
    if (patch.progress) s.progress = patch.progress;
    s.cost += patch.cost ?? 0;
    return undefined;
  }, ctx);
  const c: StepContext = { input: running.input.input, prior: running.state.checkpoint.results, api: view(rt, { env: await rt.env(ctx), commit: toolCommit(rt) }) as never, ctx, signal: rt.signal, report };
  let out;
  try {
    out = await execute(step, c);
  } catch (err) {
    // A close or abort interrupts the step; it stays running and resumes, rather than being recorded as failed.
    if (rt.signal.aborted) throw err;
    const message = err instanceof Error ? err.message : String(err);
    return rt.commit(async (tx) => {
      const s = await record(tx, rt.conversationId, step.name);
      Object.assign(s, { status: 'failed', error: message, durationMs: rt.now() - (s.startedAt ?? rt.now()) });
      return { status: 'terminal', outcome: { status: 'failed', error: { message } } };
    }, ctx);
  }
  const results = { ...running.state.checkpoint.results, [step.name]: out.result };
  await rt.commit(async (tx) => {
    const s = await record(tx, rt.conversationId, step.name);
    Object.assign(s, { status: 'done', cost: out.cost || s.cost, durationMs: rt.now() - (s.startedAt ?? rt.now()) });
    const next = steps[i + 1];
    return next ? { status: 'running', checkpoint: { phase: next.name, results } } : { status: 'terminal', outcome: { status: 'completed', result: results } };
  }, ctx);
}

/** A workflow is a pi-durable task with one phase per step; a crash resumes at the interrupted step (WFL-1, WFL-2). */
export function defineWorkflow(def: { name: string; steps: Step[] }) {
  const { name, steps } = def;
  const names = new Set(steps.map((s) => s.name));
  if (!steps.length || names.size !== steps.length) throw new Error(`workflow ${name} needs at least one step and unique step names`);

  const task = defineTask<Input, Checkpoint, Results, object>({
    name: `quack.workflow.${name}`,
    version: 1,
    initial: () => ({ phase: steps[0].name, results: {} }),
    phases: Object.fromEntries(steps.map((step, i) => [step.name, (running: RunningTask<Input, Checkpoint, Results>, rt: Runtime, ctx: Context) => runStep(steps, i, running, rt, ctx)])) as never,
    // A step waiting on a person is closed with the run: nothing is left to resume on an answer.
    abort: (_t, rt, ctx) => rt.commit(async (tx) => {
      for (const s of (await tx.doc(WorkflowDoc, rt.conversationId)).steps) if (s.status === 'waiting') s.status = 'aborted';
      return { status: 'terminal', outcome: { status: 'aborted' } };
    }, ctx),
  });
  return { name, steps, task };
}

const pending = (s: Step): StepRecord => ({ name: s.name, kind: s.kind, status: 'pending', startedAt: null, durationMs: null, cost: 0, progress: null, error: null, ask: null, answer: null });

// A run is live until its task is terminal; the same request or a live run is joined, anything else starts a fresh run in this session.
export async function startWorkflow(harness: Harness, root: Conversation, workflow: Workflow, input: JsonValue, request: string | null = null, ctx: Context = BACKGROUND_CONTEXT): Promise<string> {
  const seen = (await harness.snapshot(WorkflowDoc, root.id, ctx))?.task ?? null;
  const live = seen !== null && (await harness.getTask(Number(seen) as never, ctx))?.state.status !== 'terminal';
  return root.commit(async (tx) => {
    const doc = await tx.doc(WorkflowDoc, root.id);
    if (doc.task && (doc.task !== seen || live || (request !== null && doc.request === request))) return doc.task;
    const created = String(await tx.createTask(workflow.task, { input }, { ownership: { kind: 'conversation' } }));
    Object.assign(doc, { name: workflow.name, task: created, request, steps: workflow.steps.map(pending) });
    return created;
  }, ctx);
}

export async function outcomeOf(harness: Harness, id: string, ctx: Context = BACKGROUND_CONTEXT) {
  return (await harness.waitForTask(Number(id) as never, ctx)).state.outcome;
}

/** Starts the workflow (see startWorkflow) and waits for its outcome. */
export async function runWorkflow(harness: Harness, root: Conversation, workflow: Workflow, input: JsonValue, request: string | null = null, ctx: Context = BACKGROUND_CONTEXT) {
  return outcomeOf(harness, await startWorkflow(harness, root, workflow, input, request, ctx), ctx);
}

/** An agent definition's binding: `"workflows": { "workflow": "<name>" }` makes a submission run that workflow instead of the agent loop. */
export type WorkflowsConfig = { workflow: string };

// Workflows this image carries; extensions add theirs here as they ship.
const available: Record<string, Workflow> = {};

export const boundWorkflow = (config: unknown, from: Record<string, Workflow> = available): Workflow => {
  const wanted = (config as Partial<WorkflowsConfig> | undefined)?.workflow;
  const found = wanted ? from[wanted] : undefined;
  if (!found) throw new Error(`unknown workflow ${wanted}; available: ${Object.keys(from).sort().join(', ') || 'none'}`);
  return found;
};

/** What the session's decision routes need: the open questions and a way to answer one. */
export const sessionDecisions = (session: { harness: Harness; root: Conversation }) => ({
  pending: async () => pendingDecisions(await session.harness.snapshot(WorkflowDoc, session.root.id, BACKGROUND_CONTEXT)),
  answer: (step: string, choice: string, by: string) => answerDecision(session.root, step, choice, by, BACKGROUND_CONTEXT),
});

export function workflowExtension(workflows: Workflow[]): Extension {
  return defineExtension({ name: 'quack.workflows', tasks: workflows.map((w) => w.task) });
}

/** The bound workflow runs in place of the agent loop: the submission's text is its input, and its results are the answer. */
export function workflowSessionExtensions(config: unknown, from?: Record<string, Workflow>): Extension[] {
  const workflow = boundWorkflow(config, from);
  const dispatch: Dispatch = async ({ harness, root }, input, requestId, wait) => {
    const id = await startWorkflow(harness, root, workflow, input, requestId ?? null);
    if (!wait) return { submissionId: id, status: 'placed' };
    const outcome = await outcomeOf(harness, id);
    if (outcome.status === 'completed') return { submissionId: id, status: 'done', answer: JSON.stringify(outcome.result) };
    return { submissionId: id, status: 'unanswered', reason: outcome.status, detail: outcome.status === 'failed' ? outcome.error.message : undefined };
  };
  return [{ ...workflowExtension([workflow]), dispatch } as DispatchingExtension];
}
