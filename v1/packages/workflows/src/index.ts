import { BACKGROUND_CONTEXT, type Context } from '@earendil-works/chord/context';
import type { JsonValue } from '@earendil-works/chord';
import { defineExtension, defineTask, defineTool, GenerationTask, hook, type Conversation, type ConversationId, type Extension, type Harness, type RunningTask, type TaskRuntime, type Tx } from '@earendil-works/pi-durable';
import type { Dispatch, DispatchingExtension, Resume } from '@quack/session';
import { Type, type Message } from '@earendil-works/pi-ai';
import { Failures } from '@quack/sdk';
import { latestRun, type Chat, type ChatSession } from './chat.ts';
import { WorkflowDoc, type StepRecord } from './doc.ts';
import { execute, view, Wait, type Step, type StepContext } from './steps.ts';

export { WorkflowDoc, watchUntil, type StepRecord } from './doc.ts';
export { latestRun, type Chat, type ChatSession } from './chat.ts';
export { Wait, type Step, type StepContext } from './steps.ts';

type Results = Record<string, JsonValue>;
// The checkpoint is the results so far: committing a step's result and moving to the next phase are one write (WFL-2).
type Checkpoint = { phase: string; results: Results };
// `resume` starts a new run at a failed step with the results the earlier steps already produced.
type Input = { input: JsonValue; resume?: { phase: string; results: Results } };
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
      (await tx.doc(Failures, rt.conversationId)).items[step.name] = { error: message, run: Number(rt.taskId), at: rt.now() };
      (await tx.doc(WorkflowDoc, rt.conversationId)).kept = running.state.checkpoint.results;
      return { status: 'terminal', outcome: { status: 'failed', error: { message } } };
    }, ctx);
  }
  // The phase reruns from its top at `until`, from the same checkpoint (pi-durable sleeps between phases, never inside one).
  if (out.result instanceof Wait) return rt.sleepUntil(out.result.until, running.state.checkpoint, ctx);
  const results = { ...running.state.checkpoint.results, [step.name]: out.result };
  await rt.commit(async (tx) => {
    const s = await record(tx, rt.conversationId, step.name);
    Object.assign(s, { status: 'done', cost: out.cost || s.cost, durationMs: rt.now() - (s.startedAt ?? rt.now()) });
    const next = steps[i + 1];
    return next ? { status: 'running', checkpoint: { phase: next.name, results } } : { status: 'terminal', outcome: { status: 'completed', result: results } };
  }, ctx);
}

/** A workflow is a pi-durable task with one phase per step; a crash resumes at the interrupted step (WFL-1, WFL-2). */
export function defineWorkflow(def: { name: string; steps: Step[]; chat?: Chat }) {
  const { name, steps, chat } = def;
  const names = new Set(steps.map((s) => s.name));
  if (!steps.length || names.size !== steps.length) throw new Error(`workflow ${name} needs at least one step and unique step names`);

  const task = defineTask<Input, Checkpoint, Results, object>({
    name: `quack.workflow.${name}`,
    version: 1,
    initial: (i) => i.resume ?? { phase: steps[0].name, results: {} },
    phases: Object.fromEntries(steps.map((step, i) => [step.name, (running: RunningTask<Input, Checkpoint, Results>, rt: Runtime, ctx: Context) => runStep(steps, i, running, rt, ctx)])) as never,
    // A step waiting on a person is closed with the run: nothing is left to resume on an answer.
    abort: (_t, rt, ctx) => rt.commit(async (tx) => {
      for (const s of (await tx.doc(WorkflowDoc, rt.conversationId)).steps) if (s.status === 'waiting') s.status = 'aborted';
      return { status: 'terminal', outcome: { status: 'aborted' } };
    }, ctx),
  });
  return { name, steps, task, chat };
}

const pending = (s: Step): StepRecord => ({ name: s.name, kind: s.kind, status: 'pending', startedAt: null, durationMs: null, cost: 0, progress: null, error: null });

// A run is live until its task is terminal; the same request or a live run is joined, anything else starts a fresh run in this session.
export async function startWorkflow(harness: Harness, root: Conversation, workflow: Workflow, input: JsonValue, request: string | null = null, ctx: Context = BACKGROUND_CONTEXT): Promise<string> {
  const seen = (await harness.snapshot(WorkflowDoc, root.id, ctx))?.task ?? null;
  const live = seen !== null && (await harness.getTask(Number(seen) as never, ctx))?.state.status !== 'terminal';
  return root.commit(async (tx) => {
    const doc = await tx.doc(WorkflowDoc, root.id);
    if (doc.task && (doc.task !== seen || live || (request !== null && doc.request === request))) return doc.task;
    const created = String(await tx.createTask(workflow.task, { input }, { ownership: { kind: 'conversation' } }));
    Object.assign(doc, { name: workflow.name, task: created, request, steps: workflow.steps.map(pending), kept: null });
    (await tx.doc(Failures, root.id)).items = Object.fromEntries(workflow.steps.map((s) => [s.name, { error: null, run: Number(created), at: 0 }]));
    return created;
  }, ctx);
}

// Starts a new run at the failed step, keeping earlier results; null if the latest run did not fail.
// A failed task is terminal, so pi-durable's retryTask (parked tasks only) cannot rerun it.
export async function resumeWorkflow(harness: Harness, root: Conversation, workflow: Workflow, ctx: Context = BACKGROUND_CONTEXT): Promise<string | null> {
  const seen = await harness.snapshot(WorkflowDoc, root.id, ctx);
  const old = seen?.task ? await harness.getTask(Number(seen.task) as never, ctx) : undefined;
  const failed = seen?.steps.find((s) => s.status === 'failed');
  if (!seen || !old || !failed || old.state.status !== 'terminal') return null;
  const from = workflow.steps.findIndex((s) => s.name === failed.name);
  if (from < 0) return null;
  const input = (old.input as Input).input;
  return root.commit(async (tx) => {
    const doc = await tx.doc(WorkflowDoc, root.id);
    // Another resume or run got there first.
    if (doc.task !== seen.task) return doc.task;
    // A run that failed before `kept` existed has none: restart at the first earlier step with no stored result (null counts as a result).
    const kept = doc.kept ?? {};
    const missing = workflow.steps.slice(0, from).findIndex((s) => !Object.hasOwn(kept, s.name));
    const start = missing < 0 ? from : missing;
    const created = String(await tx.createTask(workflow.task, { input, resume: { phase: workflow.steps[start].name, results: kept } }, { ownership: { kind: 'conversation' } }));
    doc.task = created;
    doc.steps.splice(start, doc.steps.length, ...workflow.steps.slice(start).map(pending));
    (await tx.doc(Failures, root.id)).items[failed.name] = { error: null, run: Number(created), at: 0 };
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

export function workflowExtension(workflows: Workflow[]): Extension {
  return defineExtension({ name: 'quack.workflows', tasks: workflows.map((w) => w.task) });
}

const text = (t: string, isError = false) => ({ output: [{ type: 'text' as const, text: t }], isError });

// The rerun a person asks for in chat starts the job as Run does: a fresh run on the earlier run's input, joined if one is live.
const rerunTool = (workflow: Workflow, session: () => ChatSession) => defineTool({
  name: 'rerun_job',
  description: "Start this session's job again on the same input, as the Run button does. The job takes no hint, so the request text does not change it; tell the person that. Returns at once; the new result appears in this session when the run ends.",
  parameters: Type.Object({}),
  async execute(_args, api, ctx) {
    const s = session();
    const input = (await latestRun(s, ctx))?.input;
    if (input === undefined) return text('There is no earlier run to repeat.', true);
    return text(`Started run ${await startWorkflow(s.harness, s.root, workflow, input, `chat:${api.now()}`, ctx)}.`);
  },
});

// A workflow-bound session's loop runs only for chat (model steps call the model directly), so the chat prompt replaces the job prompt in each loop request.
// The instructions section is built into the agent and cannot be wrapped, so the request's system message is rewritten instead.
const asChat = (prompt: string) => (request: { readonly messages: readonly Message[] }) => ({
  messages: request.messages.map((m) => {
    const sections = (m as { sections?: Record<string, string> }).sections;
    return m.role === 'system' && sections?.instructions !== undefined ? { ...m, sections: { ...sections, instructions: `<instructions>\n${prompt}\n</instructions>` } } : m;
  }),
});

function chatExtension(workflow: Workflow, chat: Chat, session: () => ChatSession): Extension {
  return defineExtension({
    name: 'quack.workflow-chat',
    tools: [...chat.tools(session), rerunTool(workflow, session)],
    hooks: [hook(GenerationTask, { beforeRequest: asChat(chat.prompt) })],
  });
}

/** The bound workflow runs in place of the agent loop: the submission's text is its input, and its results are the answer. A message with no requestId is a person's, not a start, and goes to the chat loop when the workflow has one. */
export function workflowSessionExtensions(config: unknown, from?: Record<string, Workflow>): Extension[] {
  const workflow = boundWorkflow(config, from);
  let current: ChatSession | undefined;
  const dispatch: Dispatch = async (session, input, requestId, wait) => {
    current = session;
    if (workflow.chat && requestId === undefined) return undefined;
    const id = await startWorkflow(session.harness, session.root, workflow, input, requestId ?? null);
    if (!wait) return { submissionId: id, status: 'placed' };
    const outcome = await outcomeOf(session.harness, id);
    if (outcome.status === 'completed') return { submissionId: id, status: 'done', answer: JSON.stringify(outcome.result) };
    return { submissionId: id, status: 'unanswered', reason: outcome.status, detail: outcome.status === 'failed' ? outcome.error.message : undefined };
  };
  const resume: Resume = async ({ harness, root }) => {
    const id = await resumeWorkflow(harness, root, workflow);
    return id ? { submissionId: id, status: 'placed' } : null;
  };
  const here = () => current ?? (() => { throw new Error('chat tool ran before any submission'); })();
  return [{ ...workflowExtension([workflow]), dispatch, resume } as DispatchingExtension, ...(workflow.chat ? [chatExtension(workflow, workflow.chat, here)] : [])];
}
export { manifest } from './manifest.ts';
