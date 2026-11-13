import type { Context } from '@earendil-works/chord/context';
import { contentText, validateToolArguments, type Message } from '@earendil-works/pi-ai';
import type { Schema } from '@earendil-works/pi-schema';
import type { JsonValue } from '@earendil-works/chord';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { feedback, judged, type Judged, type Verdict } from '@quack/judge';
import { Artifacts, decide, observeGeneration, person as sdkPerson, seal, SealRefused, sealedVersion, type ArtifactKind, type DecisionPoint, type Resolver } from '@quack/sdk';
import { WorkflowDoc } from './doc.ts';

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
  | { name: string; kind: 'model'; prompt(c: StepContext): string; schema: Schema; model?: { provider: string; modelId: string } }
  /** A model answer built into a sealed `artifact`, judged on its seal when the agent enables the judge; a failing verdict goes back to the model until the judge passes or releases a version. */
  | { name: string; kind: 'judged'; artifact: ArtifactKind; prompt(c: StepContext): string; schema: Schema; model?: { provider: string; modelId: string }; card(c: StepContext, decision: JsonValue): object | Promise<object>; judge(c: StepContext): Judged }
  /** Resolvers run in order; a person answers through the session's questions when they all escalate. */
  | { name: string; kind: 'decision'; point(c: StepContext): Omit<DecisionPoint, 'name'>; resolvers?: Resolver[] };

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

type Ask = { name: string; schema: Schema; model?: { provider: string; modelId: string } };

// One conversation with the model: an invalid answer, or one `check` throws on, is repaired in `messages`; the accepted reply is returned with what `check` built from it.
async function converse<B = undefined>(step: Ask, c: StepContext, messages: Message[], check?: (args: JsonValue) => B | Promise<B>) {
  const agent = await c.api.agent(c.ctx);
  const ref = step.model ?? agent.model;
  const model = ref && c.api.models.getModel(ref.provider, ref.modelId);
  if (!model) throw new Error(`step ${step.name}: model is not available`);
  let cost = 0;
  for (let repairs = 0; ; repairs++) {
    const reply = await observeGeneration({ provider: model.provider, signal: c.signal, systemPrompt: agent.instructions ?? undefined, messages }, (r) => c.api.models.completeSimple(model, r, { signal: c.signal }));
    // A refused or failed call comes back as an empty reply with the host's error; without this it reads as "no JSON object".
    if (reply.stopReason === 'error') throw new Error(`step ${step.name}: model call failed: ${reply.errorMessage ?? 'no error given'}`);
    cost += reply.usage?.cost?.total ?? 0;
    try {
      const args = validateToolArguments({ name: step.name, description: step.name, parameters: step.schema }, { type: 'toolCall', id: step.name, name: step.name, arguments: parseObject(contentText(reply.content)) as never });
      return { result: args as JsonValue, built: (await check?.(args as JsonValue)) as B, reply, cost };
    } catch (err) {
      if (c.signal.aborted) throw err;
      const error = err instanceof Error ? err.message : String(err);
      if (repairs >= REPAIR_TURNS) throw new Error(`step ${step.name}: ${error}`);
      messages.push(reply, { role: 'user', content: `That answer was rejected:\n${error}\nAnswer again with one corrected JSON object and nothing else.`, timestamp: c.api.now() });
    }
  }
}

const askOf = (step: Ask & { prompt(c: StepContext): string }, c: StepContext): Message[] => [
  { role: 'user', content: `${step.prompt(c)}\n\nAnswer with one JSON object matching this schema and nothing else:\n${JSON.stringify(step.schema)}`, timestamp: c.api.now() },
];

async function modelStep(step: Extract<Step, { kind: 'model' }>, c: StepContext): Promise<StepOutcome> {
  const { result, cost } = await converse(step, c, askOf(step, c));
  return { result, cost };
}

/** `failed`: the judge sent the version back with this feedback. */
type Revision = { version: number; reply: JsonValue; cost: number; failed?: string };

type Answer = { built: JsonValue; reply: JsonValue; cost: number };

// The answer is memoized before its seal and the finished round after, so a crash while judging reseals the same answer, as the same version.
// An undecided judge or a deny fails the step without finishing the round, so a retry judges that version again.
async function revise(step: Extract<Step, { kind: 'judged' }>, c: StepContext, messages: Message[], n: number): Promise<Revision> {
  const key = `judged:${step.name}:${n}`;
  const done = await c.api.memo<Revision>(key, c.ctx);
  if (done) return done;
  const asked = async (): Promise<Answer> => {
    const { built, reply, cost } = await converse(step, c, messages, (d) => step.card(c, d));
    return JSON.parse(JSON.stringify({ built, reply, cost }));
  };
  const answer = (await c.api.memo<Answer>(`${key}:answer`, c.ctx)) ?? (await c.api.memo<Answer>(`${key}:answer`, await asked(), c.ctx));
  await c.api.env!.writeFile(step.artifact.path, JSON.stringify(answer.built, null, 1), c.ctx);
  judged(step.artifact.kind, step.judge(c));
  const refused = await seal(c.api, step.artifact, c.ctx).then(() => undefined, (err) => {
    if (err instanceof SealRefused) return err;
    throw err;
  });
  const sealed = (await c.api.snapshot(Artifacts, c.api.conversationId, c.ctx))!.kinds[step.artifact.kind].versions.at(-1)!;
  if (refused && (refused.verdict.kind === 'deny' || !sealed.verdict)) throw new Error(refused.message);
  return c.api.memo<Revision>(key, { version: sealed.version, reply: answer.reply, cost: answer.cost, ...(refused && { failed: refused.message }) }, c.ctx);
}

// A failed version's notes go back like a rejected answer would: the model's own reply, then what was wrong with it.
async function judgedStep(step: Extract<Step, { kind: 'judged' }>, c: StepContext): Promise<StepOutcome> {
  const messages = askOf(step, c);
  let cost = 0;
  let last = 0;
  for (let revisions = 0; ; revisions++) {
    const r = await revise(step, c, messages, revisions);
    cost += r.cost;
    const verdict = (await sealedVersion(c.api, step.artifact.kind, r.version, c.ctx))!.verdict as Verdict | null;
    // An unchanged answer reseals the same version, whose verdict cannot change: the step ends with it.
    if (!r.failed || r.version === last) return { result: { version: r.version, revisions, passed: verdict?.passed ?? null, score: verdict?.score ?? null, gatesFailed: verdict?.gatesFailed ?? [] }, cost };
    last = r.version;
    const lines = feedback(verdict!.checks).map((l) => `- ${l}`).join('\n');
    messages.push(r.reply as unknown as Message, { role: 'user', content: `The ${step.artifact.kind} built from that answer failed its checks:\n${lines}\nAnswer again with one JSON object that fixes each point and nothing else.`, timestamp: c.api.now() });
  }
}

// The sdk's person resolver, with the step shown as waiting while a question is open.
function person(step: string, c: StepContext): Resolver {
  const ask = sdkPerson(c.signal);
  const mark = (status: 'waiting' | 'running') => c.api.commit(async (tx) => { (await tx.doc(WorkflowDoc, c.api.conversationId)).steps.find((r) => r.name === step)!.status = status; }, c.ctx);
  return {
    id: ask.id,
    async resolve(point, api, ctx) {
      await mark('waiting');
      const answer = await ask.resolve(point, api, ctx);
      await mark('running');
      return answer;
    },
  };
}

async function decisionStep(step: Extract<Step, { kind: 'decision' }>, c: StepContext): Promise<StepOutcome> {
  const decision = await decide(c.api, { ...step.point(c), name: step.name }, [...(step.resolvers ?? []), person(step.name, c)], c.ctx);
  if (!decision) throw new Error(`step ${step.name}: no decision`);
  return { result: { ...decision }, cost: 0 };
}

export async function execute(step: Step, c: StepContext): Promise<StepOutcome> {
  if (step.kind === 'code') return { result: (await step.run(c)) as JsonValue, cost: 0 };
  if (step.kind === 'model') return modelStep(step, c);
  if (step.kind === 'judged') return judgedStep(step, c);
  return decisionStep(step, c);
}
