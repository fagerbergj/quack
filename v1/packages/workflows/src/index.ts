import { BACKGROUND_CONTEXT, type Context } from '@earendil-works/chord/context';
import type { JsonValue } from '@earendil-works/chord';
import { defineExtension, defineTask, type Conversation, type ConversationId, type Extension, type Harness, type RunningTask, type TaskRuntime, type Tx } from '@earendil-works/pi-durable';
import { WorkflowDoc, type StepRecord } from './doc.ts';
import { execute, view, type Step, type StepContext } from './steps.ts';

export { answerDecision, WorkflowDoc, watchUntil, type StepRecord } from './doc.ts';
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
  const c: StepContext = { input: running.input.input, prior: running.state.checkpoint.results, api: view(rt, { env: await rt.env(ctx), commit: toolCommit(rt) }) as never, ctx, signal: rt.signal };
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
    Object.assign(s, { status: 'done', cost: out.cost, durationMs: rt.now() - (s.startedAt ?? rt.now()) });
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
    abort: (_t, rt, ctx) => rt.commit(() => ({ status: 'terminal', outcome: { status: 'aborted' } }), ctx),
  });
  return { name, steps, task };
}

const pending = (s: Step): StepRecord => ({ name: s.name, kind: s.kind, status: 'pending', startedAt: null, durationMs: null, cost: 0, error: null, ask: null, answer: null });

/** Runs the workflow as the conversation's one run and waits for its outcome; a resubmission joins the run already there. */
export async function runWorkflow(harness: Harness, root: Conversation, workflow: Workflow, input: JsonValue, ctx: Context = BACKGROUND_CONTEXT) {
  const id = await root.commit(async (tx) => {
    const doc = await tx.doc(WorkflowDoc, root.id);
    if (doc.task) return doc.task;
    const created = String(await tx.createTask(workflow.task, { input }, { ownership: { kind: 'conversation' } }));
    Object.assign(doc, { name: workflow.name, task: created, steps: workflow.steps.map(pending) });
    return created;
  }, ctx);
  return (await harness.waitForTask(Number(id) as never, ctx)).state.outcome;
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

export function workflowExtension(workflows: Workflow[]): Extension {
  return defineExtension({ name: 'quack.workflows', tasks: workflows.map((w) => w.task) });
}

export function workflowSessionExtensions(config: unknown, from?: Record<string, Workflow>): Extension[] {
  return [workflowExtension([boundWorkflow(config, from)])];
}
