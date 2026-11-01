import type { Context } from '@earendil-works/chord/context';
import { contentText, validateToolArguments, type Message, type TSchema } from '@earendil-works/pi-ai';
import type { JsonValue } from '@earendil-works/chord';
import type { JsonObject, ToolExecutionApi, ToolRegistration } from '@earendil-works/pi-durable';
import { decide, gated, whileWaiting, type DecisionPoint, type Resolver } from '@quack/sdk';
import { WorkflowDoc, watchUntil } from './doc.ts';

/** What a step sees: the workflow input, the results of earlier steps by name, and the task's api for artifacts and other durable calls. */
export type StepContext = {
  input: JsonValue;
  prior: Record<string, JsonValue>;
  api: ToolExecutionApi;
  ctx: Context;
  signal: AbortSignal;
  /** Commits progress to the step's record now; `cost` adds to what the step has spent so far. */
  report(patch: { progress?: { done: number; total: number }; cost?: number }): Promise<void>;
};

/** Returned by a code step to sleep durably until `until` (ms, the task clock) and run the same step again; nothing is held meanwhile. */
export class Wait {
  until: number;
  constructor(until: number) {
    this.until = until;
  }
}

export type Step =
  | { name: string; kind: 'code'; run(c: StepContext): JsonValue | Wait | Promise<JsonValue | Wait> }
  /** The model's reply must be one JSON object matching `schema`; anything else fails the step. */
  | { name: string; kind: 'model'; prompt(c: StepContext): string; schema: TSchema; model?: { provider: string; modelId: string } }
  /** Resolvers run in order; a person answers through `answerDecision` when they all escalate. */
  | { name: string; kind: 'decision'; point(c: StepContext): Omit<DecisionPoint, 'name'>; resolvers?: Resolver[] }
  /** The tool waits for an approval before it runs; args may read earlier results. */
  | { name: string; kind: 'gated'; tool: ToolRegistration; args(c: StepContext): JsonObject };

/** The api with some members replaced; methods stay bound to the original. */
export function view<A extends object>(api: A, over: Record<string, unknown>): A {
  return new Proxy(api, {
    get(t, k) {
      if (k in over) return over[k as string];
      const v: unknown = Reflect.get(t, k);
      return typeof v === 'function' ? v.bind(t) : v;
    },
  });
}

export type StepOutcome = { result: JsonValue; cost: number };

function parseObject(text: string): unknown {
  const start = text.indexOf('{');
  const end = text.lastIndexOf('}');
  if (start < 0 || end <= start) throw new Error('model reply has no JSON object');
  try {
    return JSON.parse(text.slice(start, end + 1));
  } catch {
    throw new Error('model reply is not valid JSON');
  }
}

// An invalid answer gets its error back like a bad tool call would; a valid answer is never re-asked.
const REPAIR_TURNS = 2;

async function modelStep(step: Extract<Step, { kind: 'model' }>, c: StepContext): Promise<StepOutcome> {
  const agent = await c.api.agent(c.ctx);
  const ref = step.model ?? agent.model;
  const model = ref && c.api.models.getModel(ref.provider, ref.modelId);
  if (!model) throw new Error(`step ${step.name}: model is not available`);
  const ask = `${step.prompt(c)}\n\nAnswer with one JSON object matching this schema and nothing else:\n${JSON.stringify(step.schema)}`;
  const messages: Message[] = [{ role: 'user', content: ask, timestamp: c.api.now() }];
  let cost = 0;
  for (let repairs = 0; ; repairs++) {
    const reply = await c.api.models.completeSimple(model, { systemPrompt: agent.instructions ?? undefined, messages }, { signal: c.signal });
    // A refused or failed call comes back as an empty reply with the host's error; without this it reads as "no JSON object".
    if (reply.stopReason === 'error') throw new Error(`step ${step.name}: model call failed: ${reply.errorMessage ?? 'no error given'}`);
    cost += reply.usage?.cost?.total ?? 0;
    try {
      const args = validateToolArguments({ name: step.name, description: step.name, parameters: step.schema }, { type: 'toolCall', id: step.name, name: step.name, arguments: parseObject(contentText(reply.content)) as never });
      return { result: args as JsonValue, cost };
    } catch (err) {
      if (c.signal.aborted) throw err;
      const error = err instanceof Error ? err.message : String(err);
      if (repairs >= REPAIR_TURNS) throw new Error(`step ${step.name}: ${error}`);
      messages.push(reply, { role: 'user', content: `That answer was rejected:\n${error}\nAnswer again with one corrected JSON object and nothing else.`, timestamp: c.api.now() });
    }
  }
}

// Last resolver: records the question on the workflow document and waits for a person's answer.
function person(step: string, c: StepContext): Resolver {
  return {
    id: 'person',
    async resolve(point) {
      const { api, ctx } = c;
      await api.commit(async (tx) => {
        const s = (await tx.doc(WorkflowDoc, api.conversationId)).steps.find((r) => r.name === step)!;
        s.status = 'waiting';
        s.ask = { question: point.question, options: point.options, evidence: point.evidence ?? null, at: s.ask?.at ?? api.now() };
      }, ctx);
      const watch = await api.watchDoc(WorkflowDoc, api.conversationId, ctx);
      if (!watch) throw new Error('workflow state is unavailable');
      const answer = await whileWaiting(() => watchUntil(watch, (d) => d.steps.find((r) => r.name === step)?.answer, c.signal));
      await api.commit(async (tx) => { (await tx.doc(WorkflowDoc, api.conversationId)).steps.find((r) => r.name === step)!.status = 'running'; }, ctx);
      return { choice: answer.choice, confidence: 1, reason: `answered by ${answer.by}` };
    },
  };
}

async function decisionStep(step: Extract<Step, { kind: 'decision' }>, c: StepContext): Promise<StepOutcome> {
  const decision = await decide(c.api, { ...step.point(c), name: step.name }, [...(step.resolvers ?? []), person(step.name, c)], c.ctx);
  if (!decision) throw new Error(`step ${step.name}: no decision`);
  return { result: { ...decision }, cost: 0 };
}

async function gatedStep(step: Extract<Step, { kind: 'gated' }>, c: StepContext): Promise<StepOutcome> {
  // Approvals are keyed by task id; a workflow has one task and many steps, so each step gets its own key.
  const api = view(c.api, { taskId: `${String(c.api.taskId)}:${step.name}` });
  const out = await gated(step.tool).execute(step.args(c) as never, api, c.ctx);
  const text = (out.output ?? []).map((p) => (p.type === 'text' ? p.text : '')).join('');
  if (out.isError) throw new Error(`step ${step.name}: ${text}`);
  return { result: out.structuredOutput ?? text, cost: out.usage?.cost?.total ?? 0 };
}

export async function execute(step: Step, c: StepContext): Promise<StepOutcome> {
  if (step.kind === 'code') return { result: (await step.run(c)) as JsonValue, cost: 0 };
  if (step.kind === 'model') return modelStep(step, c);
  if (step.kind === 'decision') return decisionStep(step, c);
  return gatedStep(step, c);
}
