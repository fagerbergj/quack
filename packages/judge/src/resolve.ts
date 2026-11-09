import type { Context } from '@earendil-works/chord/context';
import { contentText, type ClassifierModel, type ClassifierQuestion } from '@earendil-works/pi-ai';
import type { JsonObject, ToolExecutionApi } from '@earendil-works/pi-durable';
import { createHash } from 'node:crypto';
import { agentModel, decide, modelResolver, observeGeneration, parseDecision } from '@quack/sdk';
import type { ModelSplit, Question } from './declaration.ts';
import { limiter } from './limit.ts';
import type { Unit } from './rubric.ts';

/** A decision model such as Clef, reached through pi-ai's classifier API. */
export type ClassifierRef = { provider: string; modelId: string; maxInputTokens: number; act?: number };

export type Answer = { met: boolean | null; resolver: string; note?: string };

// Clef's tokenizer counted 9.6-10.6k tokens on code units the 4-byte rule put under 8,192 (spike #2190); code packs ~3 bytes a token.
const BYTES_PER_TOKEN = 3;

/** One question asked of a unit: `id` is the check's key; a choice question holds when the answer is one of `pass` (its first option by default). */
export type Ask = { id: string; question: Question; pass?: string[] };

/** A unit with the questions asked of it. */
export type Item = { unit: Unit; checks: Ask[] };

export type Limit = ReturnType<typeof limiter>;

const CRITERIA = { true: 'Yes, this holds.', false: 'No, it does not hold.' };
// A unit without shared context goes alone with one question per check; packed units share one context and are told apart by index.
const qid = (ask: Ask, i: number, packed: boolean) => (packed ? `${ask.id}#${i}` : ask.id);
const passing = (ask: Ask): string[] => (ask.question.type === 'choice' ? ask.pass ?? Object.keys(ask.question.options).slice(0, 1) : ask.question.pass ?? ['yes']);
/** The answers that fail an item ("yes" for a pitfall asked as the defect), for a note when the model gave no reason. */
export const failing = (ask: Ask) => (ask.question.type === 'choice' ? Object.keys(ask.question.options) : ['yes', 'no']).filter((o) => !passing(ask).includes(o)).join(' or ');

function asked(ask: Ask, instructions: string): ClassifierQuestion {
  return ask.question.type === 'choice' ? { type: 'choice', instructions, criteria: ask.question.options } : { type: 'bool', instructions, criteria: CRITERIA };
}

function request(items: Item[]) {
  const shared = items[0].unit.shared;
  const questions: Record<string, ClassifierQuestion> = Object.fromEntries(items.flatMap((it, i) => it.checks.map((c) => [
    qid(c, i, !!shared),
    asked(c, shared ? `About items[${i}] (${it.unit.id}): ${c.question.instructions}` : c.question.instructions),
  ])));
  const state: JsonObject = shared ? { context: shared, items: items.map((it) => ({ id: it.unit.id, ...it.unit.state })) } : items[0].unit.state;
  return { state, questions };
}

/** The decision model's input tokens for one request (one unit, or units packed with their shared context), as the cap is judged. */
export const classifierTokens = (unit: Unit | Unit[], checks: Ask[]) =>
  JSON.stringify(request((Array.isArray(unit) ? unit : [unit]).map((u) => ({ unit: u, checks })))).length / BYTES_PER_TOKEN;
const tokensOf = (items: Item[]) => JSON.stringify(request(items)).length / BYTES_PER_TOKEN;

/** Units that share context are packed in order into requests up to the cap; those that alone are over it go together, to escalate as one. */
export function pack(items: Item[], cap: number | undefined): Item[][] {
  const out: Item[][] = [];
  const open = new Map<string, Item[]>();
  const over = new Map<string, Item[]>();
  for (const it of items) {
    const group = it.unit.shared && cap ? JSON.stringify(it.unit.shared) : undefined;
    const big = group !== undefined && tokensOf([it]) > cap! ? over.get(group) : undefined;
    if (big) {
      big.push(it);
      continue;
    }
    const current = group === undefined ? undefined : open.get(group);
    if (current && tokensOf([...current, it]) <= cap!) {
      current.push(it);
      continue;
    }
    const fresh = [it];
    out.push(fresh);
    if (group !== undefined) (tokensOf([it]) > cap! ? over : open).set(group, fresh);
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
async function classify(api: ToolExecutionApi, ref: ClassifierRef, items: Item[], clef: Limit, ctx: Context): Promise<Record<string, number> | null | undefined> {
  // The decision model has a hard input cap; over it, every question escalates instead of being truncated.
  if (tokensOf(items) > ref.maxInputTokens) return null;
  const model = api.models.getAllModels(ref.provider).find((m) => m.id === ref.modelId && m.type === 'classifier') as ClassifierModel<never> | undefined;
  if (!model) return null;
  const result = await clef(() => api.models.classify(model, request(items), { signal: ctx.abortSignal }).catch(() => undefined));
  ctx.abortSignal?.throwIfAborted();
  if (result?.stopReason !== 'stop') return undefined;
  const asks = new Map(items.flatMap((it, i) => it.checks.map((c) => [qid(c, i, !!items[0].unit.shared), c] as const)));
  return Object.fromEntries(Object.entries(result.answers).flatMap(([id, a]) => {
    const ask = asks.get(id);
    const p = ask && massOf(ask, a as never);
    return p === undefined ? [] : [[id, p]];
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
type Pending = { i: number; unit: Unit; check: Ask };

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

// One judge-model request for a packed group's unsure questions, memoized per call; one it leaves unanswered escalates alone.
async function escalatePacked(api: ToolExecutionApi, s: Settings, pending: Pending[], ctx: Context): Promise<Answer[]> {
  const key = `${s.memo}:escalation:${createHash('sha256').update(pending.map((p) => `${p.unit.id}:${p.check.id}`).join('\n')).digest('hex').slice(0, 16)}`;
  let replies = await api.memo<Array<{ choice: string; reason: string } | null>>(key, ctx);
  if (!replies) {
    const ref = s.judgeModel ?? (await api.agent(ctx)).model;
    const model = ref && api.models.getModel(ref.provider, ref.modelId);
    const messages = [{ role: 'user' as const, content: packedPrompt(pending), timestamp: api.now() }];
    const reply = model && (await observeGeneration({ provider: model.provider, signal: ctx.abortSignal, messages }, (r) => api.models.completeSimple(model, r, { signal: ctx.abortSignal })));
    ctx.abortSignal?.throwIfAborted();
    replies = await api.memo(key, reply && reply.stopReason !== 'error' ? parsePacked(contentText(reply.content), pending) : pending.map(() => null), ctx);
  }
  const resolver = s.judgeModel ? 'judge-model' : 'agent-model';
  return Promise.all(pending.map((p, n) => (replies![n] ? { met: passing(p.check).includes(replies![n]!.choice), resolver, note: replies![n]!.reason } : escalate(api, s.judgeModel, s.memo, p.unit, p.check, ctx))));
}

async function escalate(api: ToolExecutionApi, judgeModel: ModelRef | undefined, memo: string, unit: Unit, ask: Ask, ctx: Context): Promise<Answer> {
  const resolver = judgeModel ? { ...modelResolver(judgeModel.provider, judgeModel.modelId), id: 'judge-model' } : agentModel;
  const d = await decide(api, point(memo, unit, ask), [resolver], ctx);
  if (!d) return { met: null, resolver: 'none', note: 'no resolver could decide' };
  return { met: passing(ask).includes(d.choice), resolver: d.resolver, note: d.reason };
}

export type Settings = { ref?: ClassifierRef; judgeModel?: ModelRef; memo: string; clef: Limit; escalations: Limit };
/** `answer` is absent when the item was not escalated because its check had already decided. */
export type Answered = { unit: Unit; check: Ask; answer?: Answer };
/** Whether an unsure item still needs the judge model: false once its check's fold has decided. */
export type Need = (unit: Unit, check: Ask) => boolean;

async function probabilitiesOf(api: ToolExecutionApi, s: Settings, items: Item[], ctx: Context) {
  const key = items.length === 1 ? `${s.memo}:${items[0].unit.id}:classifier` : `${s.memo}:${items[0].unit.id}+${items.length}:classifier`;
  const stored = await api.memo<Record<string, number> | null>(key, ctx);
  if (stored !== undefined) return stored;
  const fresh = s.ref ? await classify(api, s.ref, items, s.clef, ctx) : null;
  return fresh === undefined ? undefined : api.memo(key, fresh, ctx);
}

// A packed group's unsure questions, the ones whose check can still change, go to the judge model in one request within the limit.
async function escalateTogether(api: ToolExecutionApi, s: Settings, pending: Pending[], ctx: Context, need: Need): Promise<Array<Answer | undefined>> {
  if (!pending.some((p) => need(p.unit, p.check))) return pending.map(() => undefined);
  return s.escalations(async () => {
    const still = pending.filter((p) => need(p.unit, p.check));
    const answers = still.length ? await escalatePacked(api, s, still, ctx) : [];
    return pending.map((p) => answers[still.indexOf(p)]);
  });
}

// One decision-model request per packed group; confident answers stand and are counted first, so a decided check asks no more.
// The rest escalate: a packed group's together in one request, a lone unit's questions concurrently, all within the escalation limit.
export async function answerItems(api: ToolExecutionApi, s: Settings, items: Item[], ctx: Context, onAnswer?: (a: Answered) => Promise<void>, need: Need = () => true): Promise<Answered[]> {
  const packed = !!items[0].unit.shared;
  const probabilities = await probabilitiesOf(api, s, items, ctx);
  const act = s.ref?.act ?? 0.8;
  const asked = items.flatMap((it, i) => it.checks.map((check) => ({ i, unit: it.unit, check, p: probabilities?.[qid(check, i, packed)] })));
  const sure = (p?: number) => p !== undefined && (p >= act || 1 - p >= act);
  const out = new Map<Pending, Answered>();
  const done = async (a: Pending, answer: Answer | undefined) => {
    const answered = { unit: a.unit, check: a.check, answer };
    out.set(a, answered);
    await onAnswer?.(answered);
  };
  for (const a of asked.filter((x) => sure(x.p))) await done(a, { met: a.p! >= act, resolver: s.ref!.modelId });
  const unsure = asked.filter((x) => !sure(x.p));
  const alone = (a: Pending) => (need(a.unit, a.check) ? s.escalations(() => (need(a.unit, a.check) ? escalate(api, s.judgeModel, s.memo, a.unit, a.check, ctx) : Promise.resolve(undefined))) : undefined);
  if (packed && unsure.length > 1) {
    const answers = await escalateTogether(api, s, unsure, ctx, need);
    for (const [n, a] of unsure.entries()) await done(a, answers[n]);
  } else await Promise.all(unsure.map(async (a) => done(a, await alone(a))));
  return asked.map((a) => out.get(a)!);
}

const ITEMS = /\[[\s\S]*\]/;

function parseItems(text: string): Array<{ item: string; span: string }> | undefined {
  try {
    const list = JSON.parse(ITEMS.exec(text)?.[0] ?? 'null') as unknown;
    if (!Array.isArray(list) || !list.every((x) => typeof x?.item === 'string' && typeof x?.span === 'string')) return undefined;
    return list.map(({ item, span }) => ({ item, span }));
  } catch {
    return undefined;
  }
}

const splitPrompt = (text: string, instruction = 'one distinct claim, ask or promise') =>
  `Split the text below into items. An item is: ${instruction}.\nFor each item, state it in one sentence and quote the exact span of the text it comes from.\nAnswer with one JSON array and nothing else: [{"item": "<the item>", "span": "<exact quote from the text>"}]\n\nText:\n${text}`;

/** The LLM splitter's model call: the judge model (the agent's own when unset), its split memoized per sealed version and scope. */
export function modelSplit(api: ToolExecutionApi, judgeModel: ModelRef | undefined, memo: string, ctx: Context): ModelSplit {
  return async (text, o, role) => {
    const key = `${memo}:split:${role}`;
    const stored = await api.memo<Array<{ item: string; span: string }>>(key, ctx);
    if (stored) return stored;
    const ref = judgeModel ?? (await api.agent(ctx)).model;
    const model = ref && api.models.getModel(ref.provider, ref.modelId);
    if (!model) return undefined;
    const messages = [{ role: 'user' as const, content: splitPrompt(text, o.instruction), timestamp: api.now() }];
    const reply = await observeGeneration({ provider: model.provider, signal: ctx.abortSignal, messages }, (r) => api.models.completeSimple(model, r, { signal: ctx.abortSignal }));
    const items = reply.stopReason === 'error' ? undefined : parseItems(contentText(reply.content));
    return items && api.memo(key, items, ctx);
  };
}
