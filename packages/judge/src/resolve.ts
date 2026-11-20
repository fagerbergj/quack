import type { Context } from '@earendil-works/chord/context';
import { contentText, type ClassifierModel, type ClassifierQuestion } from '@earendil-works/pi-ai';
import type { JsonObject, ToolExecutionApi } from '@earendil-works/pi-durable';
import { createHash } from 'node:crypto';
import { agentModel, decide, modelResolver, observeGeneration, inputTooLarge, parseDecision, retryModelCall, type RetrySeams } from '@quack/sdk';
import { PASS_ONLY, type Question } from './declaration.ts';
import { limiter } from './limit.ts';
import type { Unit } from './rubric.ts';

/** A decision model such as Clef, reached through pi-ai's models.classify. */
export type DecisionModelRef = { provider: string; modelId: string; maxInputTokens: number; act?: number };

export type Answer = { met: boolean | null; resolver: string; note?: string };

// Clef's tokenizer packs code at about 3 bytes a token, so the usual 4-byte rule puts units over its cap below it.
const BYTES_PER_TOKEN = 3;

/** One question asked of a unit: `id` is the check's key; a choice question holds when the answer is one of `pass` (its first option by default). */
export type Ask = { id: string; question: Question; pass?: string[]; /** false: never asked of the decision model, only escalated; PASS_ONLY: only its passing answers stand. */ decisionModel?: false | typeof PASS_ONLY };

/** A unit with the questions asked of it. */
export type Item = { unit: Unit; checks: Ask[] };

export type Limit = ReturnType<typeof limiter>;

const CRITERIA = { true: 'Yes, this holds.', false: 'No, it does not hold.' };
// Part of every item-reuse key: bump it when request, point, packedPrompt, CRITERIA or the sdk's decide prompt change,
// so answers given to the old prompts are asked again.
export const PROMPT_VERSION = 2;
// A unit without shared context goes alone with one question per check; packed units share one context and are told apart by index.
const qid = (ask: Ask, i: number, packed: boolean) => (packed ? `${ask.id}#${i}` : ask.id);
const passing = (ask: Ask): string[] => (ask.question.type === 'choice' ? ask.pass ?? Object.keys(ask.question.options).slice(0, 1) : ask.question.pass ?? ['yes']);

function asked(ask: Ask, instructions: string): ClassifierQuestion {
  return ask.question.type === 'choice' ? { type: 'choice', instructions, criteria: ask.question.options } : { type: 'bool', instructions, criteria: CRITERIA };
}

/** Each question of a request with the id the judge reads its answer by, in the order the request numbers them. */
const questionsOf = (items: Item[]) => items.flatMap((it, i) => it.checks.filter((c) => c.decisionModel !== false).map((c) => ({ id: qid(c, i, !!items[0].unit.shared), i, it, c })));

// Clef reads a question id as part of the question, so questions are numbered. Its time grows with bytes, so a packed
// request names each item once, in `items`, and states a question asked of several items once, in `asks`.
function request(items: Item[]) {
  const shared = items[0].unit.shared;
  const all = questionsOf(items);
  if (!shared) return { state: items[0].unit.state, questions: Object.fromEntries(all.map(({ c }, n) => [`q${n}`, asked(c, c.question.instructions)])) };
  const repeated = [...new Set(all.map((q) => q.c.id))].filter((id) => all.filter((q) => q.c.id === id).length > 1);
  // Resolved once: the position of an id in `asks`, so the questions below do not rescan `repeated` per question.
  const asksIndex = new Map(repeated.map((id, i) => [id, i]));
  const questions: Record<string, ClassifierQuestion> = Object.fromEntries(all.map(({ i, c }, n) => [
    `q${n}`,
    asked(c, asksIndex.has(c.id) ? `About items[${i}]: answer asks[${asksIndex.get(c.id)}].` : `About items[${i}]: ${c.question.instructions}`),
  ]));
  const asks = repeated.map((id) => all.find((q) => q.c.id === id)!.c.question.instructions);
  const state: JsonObject = { context: shared, ...(asks.length ? { asks } : {}), items: items.map((it) => ({ id: it.unit.id, ...it.unit.state })) };
  return { state, questions };
}

/** The decision model's input tokens for one request (one unit, or units packed with their shared context), as the cap is judged. */
export const decisionModelTokens = (unit: Unit | Unit[], checks: Ask[]) =>
  JSON.stringify(request((Array.isArray(unit) ? unit : [unit]).map((u) => ({ unit: u, checks })))).length / BYTES_PER_TOKEN;
const tokensOf = (items: Item[]) => JSON.stringify(request(items)).length / BYTES_PER_TOKEN;

/** Units that share context are packed in order into requests up to the cap; those that skip the decision model go together, to escalate as one. */
export function pack(items: Item[], cap: number | undefined): Item[][] {
  const out: Item[][] = [];
  const open = new Map<string, Item[]>();
  const over = new Map<string, Item[]>();
  for (const it of items) {
    const group = it.unit.shared ? JSON.stringify(it.unit.shared) : undefined;
    const skips = cap === undefined || !questionsOf([it]).length || tokensOf([it]) > cap;
    const target = group === undefined ? undefined : (skips ? over : open).get(group);
    if (target && (skips || tokensOf([...target, it]) <= cap!)) {
      target.push(it);
      continue;
    }
    const fresh = [it];
    out.push(fresh);
    if (group !== undefined) (skips ? over : open).set(group, fresh);
  }
  return out;
}

// A choice answer counts the probability of the options that hold; a bool answer its probability of yes, or of no when no holds.
function massOf(ask: Ask, a: { type: string; probability?: number; probabilities?: Record<string, number> }): number | undefined {
  if (a.type === 'bool' && ask.question.type === 'bool') return a.probability === undefined ? undefined : passing(ask).reduce((s, k) => s + (k === 'yes' ? a.probability! : 1 - a.probability!), 0);
  if (a.type === 'choice' && ask.question.type === 'choice') return passing(ask).reduce((s, k) => s + (a.probabilities?.[k] ?? 0), 0);
  return undefined;
}

// null is a deterministic miss (over the cap, no such model) and is memoized; undefined is an outage, retried on a rerun.
async function classify(api: ToolExecutionApi, ref: DecisionModelRef, items: Item[], s: Settings, ctx: Context): Promise<Record<string, number> | null | undefined> {
  // The decision model has a hard input cap; over it, every question escalates instead of being truncated.
  if (tokensOf(items) > ref.maxInputTokens) return null;
  // A group whose checks all skip the decision model sends it nothing.
  if (!questionsOf(items).length) return null;
  const model = api.models.getAllModels(ref.provider).find((m) => m.id === ref.modelId && m.type === 'classifier') as ClassifierModel<never> | undefined;
  if (!model) return null;
  // maxRetries 0: pi-ai's own provider retry would multiply under ours.
  const ask = (fetch: typeof globalThis.fetch) => api.models.classify(model, request(items), { signal: ctx.abortSignal, fetch, maxRetries: 0 });
  const result = await s.clef(() => retryModelCall(`${ref.provider}/${ref.modelId}`, ask, ctx.abortSignal, s.retry).catch(() => undefined));
  ctx.abortSignal?.throwIfAborted();
  // The estimate can undercount a request the model then refuses as too large: that is over the cap, not an outage.
  if (result && inputTooLarge(result)) return null;
  if (result?.stopReason !== 'stop') return undefined;
  const asks = new Map(questionsOf(items).map((q, n): [string, (typeof q)] => [`q${n}`, q]));
  return Object.fromEntries(Object.entries(result.answers).flatMap(([key, a]) => {
    const q = asks.get(key);
    const p = q && massOf(q.c, a as never);
    return q && p !== undefined ? [[q.id, p]] : [];
  }));
}

/** Where uncertain checks go: a named judge model, or the agent's own model when none is set. */
export type ModelRef = { provider: string; modelId: string };

function point(memo: string, unit: Unit, ask: Ask) {
  const state = unit.shared ? { context: unit.shared, ...unit.state } : unit.state;
  const q = ask.question;
  const question = q.type === 'choice' ? `${q.instructions}\n${Object.entries(q.options).map(([k, v]) => `- ${k}: ${v}`).join('\n')}` : q.instructions;
  return { name: `${memo}:${unit.id}:${ask.id}`, question, options: q.type === 'choice' ? Object.keys(q.options) : ['yes', 'no'], evidence: JSON.stringify(state, null, 1) };
}

const optionsOf = (q: Question) => (q.type === 'choice' ? Object.keys(q.options) : ['yes', 'no']);

/** One unsure question of a packed group: its unit, which item that unit is in the request, and the check. */
export type Pending = { i: number; unit: Unit; check: Ask };

// The context goes once, as on the decision model; the questions follow the evidence so a long context does not bury them.
function packedPrompt(pending: Pending[]): string {
  const units = [...new Set(pending.map((p) => p.unit))];
  const evidence = { context: units[0].shared, items: units.map((u) => ({ id: u.id, ...u.state })) };
  const questions = pending.map((p, n) => `- q${n}: About items[${units.indexOf(p.unit)}] (${p.unit.id}): ${point('', p.unit, p.check).question}\n  Options: ${optionsOf(p.check.question).join(', ')}`);
  return `Evidence:\n${JSON.stringify(evidence, null, 1)}\n\nDecide each question below from the evidence.\n${questions.join('\n')}\n\nAnswer with one JSON object and nothing else, one entry per question id: {"q0": {"choice": "<one option exactly>", "confidence": <0 to 1>, "reason": "<one sentence>"}, ...}`;
}

function parsePacked(text: string, pending: Pending[]): Array<{ choice: string; reason: string } | null> {
  let all: Record<string, unknown> = {};
  try {
    all = JSON.parse(text.slice(text.indexOf('{'), text.lastIndexOf('}') + 1)) ?? {};
  } catch {
    // A reply that is not JSON answers nothing; every question falls back to its own escalation.
  }
  return pending.map((p, n) => parseDecision(JSON.stringify(all[`q${n}`] ?? null), { name: '', question: '', options: optionsOf(p.check.question) }) ?? null);
}

type Reply = { choice: string; reason: string } | null;
type JudgeModel = ReturnType<ToolExecutionApi['models']['getModel']>;

/** The judge model's input window: its context less the reply it may write; undefined when the deployment gives none. */
const windowOf = (m: JudgeModel) => (m?.contextWindow ? m.contextWindow - (m.maxTokens ?? 0) : undefined);

// The judge model reasons through every question of a request in one generation, so a long batch is cut to run in parallel.
export const MAX_BATCH_QUESTIONS = 4;

/** Unsure questions in order, cut into sub-batches of at most MAX_BATCH_QUESTIONS whose prompt (the shared context once in each) fits the judge model's window. */
export function subBatches(pending: Pending[], window: number | undefined): Pending[][] {
  const out: Pending[][] = [];
  for (const p of pending) {
    const last = out.at(-1);
    const fits = (batch: Pending[]) => batch.length <= MAX_BATCH_QUESTIONS && (window === undefined || packedPrompt(batch).length / BYTES_PER_TOKEN <= window);
    if (last && fits([...last, p])) last.push(p);
    else out.push([p]);
  }
  return out;
}

/** The memo key of one batched judge-model request: its questions, so a rerun of the same sub-batch reuses its reply. */
export const batchKey = (memo: string, pending: Pending[]) => `${memo}:escalation:${createHash('sha256').update(pending.map((p) => `${p.unit.id}:${p.check.id}`).join('\n')).digest('hex').slice(0, 16)}`;

// One judge-model request for a sub-batch, memoized; fewer than two questions go alone instead, so none is sent.
async function askBatch(api: ToolExecutionApi, s: Settings, model: JudgeModel, pending: Pending[], ctx: Context): Promise<Map<Pending, Reply>> {
  if (pending.length < 2 || !model) return new Map();
  const key = batchKey(s.memo, pending);
  let replies = await api.memo<Reply[]>(key, ctx);
  if (!replies) {
    const messages = [{ role: 'user' as const, content: packedPrompt(pending), timestamp: api.now() }];
    const send = (fetch: typeof globalThis.fetch) => observeGeneration({ provider: model.provider, signal: ctx.abortSignal, messages }, (r) => api.models.completeSimple(model, r, { signal: ctx.abortSignal, fetch }));
    const reply = await retryModelCall(`${model.provider}/${model.id}`, send, ctx.abortSignal, s.retry).catch(() => undefined);
    ctx.abortSignal?.throwIfAborted();
    if (!reply || reply.stopReason === 'error') return new Map();
    replies = await api.memo(key, parsePacked(contentText(reply.content), pending), ctx);
  }
  return new Map(pending.map((p, n) => [p, replies![n]]));
}

async function escalate(api: ToolExecutionApi, s: Settings, unit: Unit, ask: Ask, ctx: Context): Promise<Answer> {
  const { judgeModel, memo } = s;
  const resolver = judgeModel ? { ...modelResolver(judgeModel.provider, judgeModel.modelId, s.retry), id: 'judge-model' } : agentModel;
  const d = await decide(api, point(memo, unit, ask), [resolver], ctx);
  if (!d) return { met: null, resolver: 'none', note: 'no resolver could decide' };
  // An escalated item counts under the modelId of the model that answered it, the judge model's or the agent's own.
  const by = (judgeModel ?? (await api.agent?.(ctx))?.model)?.modelId;
  return { met: passing(ask).includes(d.choice), resolver: by ?? d.resolver, note: d.reason };
}

/** Told when a request's decision-model answers are in, after its confident ones are counted; `ready` is what an unsure item waits on before it may escalate, or undefined when it need not wait. */
export type Hold = { heard: (asked: Pending[]) => void; ready: (unit: Unit, check: Ask) => Promise<void> | undefined };
/** `retry` is the model calls' retry test seam. */
export type Settings = { ref?: DecisionModelRef; judgeModel?: ModelRef; memo: string; clef: Limit; escalations: Limit; retry?: RetrySeams; hold?: Hold };
/** `answer` is absent when the item was not escalated because its check had already decided. */
export type Answered = { unit: Unit; check: Ask; answer?: Answer };
/** Whether an unsure item still needs the judge model: false once its check's fold has decided. */
export type Need = (unit: Unit, check: Ask) => boolean;

async function probabilitiesOf(api: ToolExecutionApi, s: Settings, items: Item[], ctx: Context) {
  // The suffix predates the decision-model name; recorded memos are keyed by it, so renaming it would re-ask them.
  const key = items.length === 1 ? `${s.memo}:${items[0].unit.id}:classifier` : `${s.memo}:${items[0].unit.id}+${items.length}:classifier`;
  const stored = await api.memo<Record<string, number> | null>(key, ctx);
  if (stored !== undefined) return stored;
  const fresh = s.ref ? await classify(api, s.ref, items, s, ctx) : null;
  return fresh === undefined ? undefined : api.memo(key, fresh, ctx);
}

// A packed group's unsure questions go in sub-batches that fit the judge model, each one request within the limit;
// a question its reply leaves out, or a failed sub-batch's, escalates alone in a slot of its own.
async function escalateGroup(api: ToolExecutionApi, s: Settings, unsure: Pending[], ctx: Context, need: Need, alone: Alone, done: Done) {
  const ref = s.judgeModel ?? (await api.agent(ctx)).model;
  const model = ref && api.models.getModel(ref.provider, ref.modelId);
  const resolver = ref?.modelId ?? agentModel.id;
  await Promise.all(subBatches(unsure, windowOf(model)).map(async (batch) => {
    const held = batch.map((p) => s.hold?.ready(p.unit, p.check)).filter((w) => w !== undefined);
    if (held.length) await Promise.all(held);
    const replies = await s.escalations(() => askBatch(api, s, model, batch.filter((p) => need(p.unit, p.check)), ctx));
    await Promise.all(batch.map(async (p) => {
      const r = replies.get(p);
      await done(p, r ? { met: passing(p.check).includes(r.choice), resolver, note: r.reason } : await alone(p));
    }));
  }));
}

type Alone = (p: Pending) => Promise<Answer | undefined> | undefined;
type Done = (p: Pending, a: Answer | undefined) => Promise<void>;

// One decision-model request per packed group; confident answers stand and are counted first, so a decided check asks no more.
// The rest escalate: a packed group's in sub-batches, a lone unit's questions concurrently, all within the escalation limit.
export async function answerItems(api: ToolExecutionApi, s: Settings, items: Item[], ctx: Context, onAnswer?: (a: Answered) => Promise<void>, need: Need = () => true): Promise<Answered[]> {
  const pairs = items.flatMap((it, i) => it.checks.map((check) => ({ i, unit: it.unit, check })));
  const out = new Map<Pending, Answered>();
  const done = async (a: Pending, answer: Answer | undefined) => {
    const answered = { unit: a.unit, check: a.check, answer };
    out.set(a, answered);
    await onAnswer?.(answered);
  };
  if (pairs.every((a) => !need(a.unit, a.check))) {
    for (const a of pairs) await done(a, undefined);
    return pairs.map((a) => out.get(a)!);
  }
  const packed = !!items[0].unit.shared;
  const probabilities = await probabilitiesOf(api, s, items, ctx);
  const act = s.ref?.act ?? 0.8;
  const asked = pairs.map((a) => ({ ...a, p: probabilities?.[qid(a.check, a.i, packed)] }));
  // A PASS_ONLY check's confident failing answer is not taken: the judge model decides it and gives the writer a reason.
  const sure = (p: number | undefined, check: Ask) => p !== undefined && (p >= act || (1 - p >= act && check.decisionModel !== PASS_ONLY));
  for (const a of asked.filter((x) => sure(x.p, x.check))) await done(a, { met: a.p! >= act, resolver: s.ref!.modelId });
  s.hold?.heard(asked);
  const unsure = asked.filter((x) => !sure(x.p, x.check));
  const go = (a: Pending) => (need(a.unit, a.check) ? s.escalations(() => (need(a.unit, a.check) ? escalate(api, s, a.unit, a.check, ctx) : Promise.resolve(undefined))) : undefined);
  // A held item takes no escalation slot while it waits.
  const alone = (a: Pending) => s.hold?.ready(a.unit, a.check)?.then(() => go(a)) ?? go(a);
  if (packed && unsure.length > 1) await escalateGroup(api, s, unsure, ctx, need, alone, done);
  else await Promise.all(unsure.map(async (a) => done(a, await alone(a))));
  return asked.map((a) => out.get(a)!);
}
