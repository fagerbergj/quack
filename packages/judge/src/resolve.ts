import type { Context } from '@earendil-works/chord/context';
import type { ClassifierModel, ClassifierQuestion } from '@earendil-works/pi-ai';
import type { JsonObject, ToolExecutionApi } from '@earendil-works/pi-durable';
import { agentModel, decide, modelResolver } from '@quack/sdk';
import type { Check, Unit } from './rubric.ts';

/** A decision model such as Clef, reached through pi-ai's classifier API. */
export type ClassifierRef = { provider: string; modelId: string; maxInputTokens: number; act?: number };

export type Answer = { met: boolean | null; resolver: string; note?: string };

// Clef's tokenizer counted 9.6-10.6k tokens on code units the 4-byte rule put under 8,192 (spike #2190); code packs ~3 bytes a token.
const BYTES_PER_TOKEN = 3;

/** A unit with the checks judged on it. */
export type Item = { unit: Unit; checks: Check[] };

import { limiter } from './limit.ts';

export type Limit = ReturnType<typeof limiter>;

const CRITERIA = { true: 'Yes, this holds.', false: 'No, it does not hold.' };
// A unit without shared context goes alone with one question per check; packed units share one context and are told apart by index.
const qid = (check: Check, i: number, packed: boolean) => (packed ? `${check.id}#${i}` : check.id);

function request(items: Item[]) {
  const shared = items[0].unit.shared;
  const questions: Record<string, ClassifierQuestion> = Object.fromEntries(items.flatMap((it, i) => it.checks.map((c) => [
    qid(c, i, !!shared),
    { type: 'bool', instructions: shared ? `About items[${i}] (${it.unit.id}): ${c.question!}` : c.question!, criteria: CRITERIA },
  ])));
  const state: JsonObject = shared ? { context: shared, items: items.map((it) => ({ id: it.unit.id, ...it.unit.state })) } : items[0].unit.state;
  return { state, questions };
}

/** The decision model's input tokens for one request (one unit, or units packed with their shared context), as the cap is judged. */
export const classifierTokens = (unit: Unit | Unit[], checks: Check[]) =>
  JSON.stringify(request((Array.isArray(unit) ? unit : [unit]).map((u) => ({ unit: u, checks })))).length / BYTES_PER_TOKEN;
const tokensOf = (items: Item[]) => JSON.stringify(request(items)).length / BYTES_PER_TOKEN;

/** Units that share context are packed in order into requests up to the cap; one that alone is over it goes by itself and escalates. */
export function pack(items: Item[], cap: number | undefined): Item[][] {
  const out: Item[][] = [];
  const open = new Map<string, Item[]>();
  for (const it of items) {
    const group = it.unit.shared && cap ? JSON.stringify(it.unit.shared) : undefined;
    const current = group === undefined ? undefined : open.get(group);
    if (current && tokensOf([...current, it]) <= cap!) {
      current.push(it);
      continue;
    }
    const fresh = [it];
    out.push(fresh);
    if (group !== undefined) open.set(group, fresh);
  }
  return out;
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
  return Object.fromEntries(Object.entries(result.answers).flatMap(([id, a]) => (a.type === 'bool' ? [[id, a.probability]] : [])));
}

/** Where uncertain checks go: a named judge model, or the agent's own model when none is set. */
export type ModelRef = { provider: string; modelId: string };

async function escalate(api: ToolExecutionApi, judgeModel: ModelRef | undefined, memo: string, unit: Unit, check: Check, ctx: Context): Promise<Answer> {
  const state = unit.shared ? { context: unit.shared, ...unit.state } : unit.state;
  const point = { name: `${memo}:${unit.id}:${check.id}`, question: check.question!, options: ['yes', 'no'], evidence: JSON.stringify(state, null, 1) };
  const resolver = judgeModel ? { ...modelResolver(judgeModel.provider, judgeModel.modelId), id: 'judge-model' } : agentModel;
  const d = await decide(api, point, [resolver], ctx);
  if (!d) return { met: null, resolver: 'none', note: 'no resolver could decide' };
  return { met: d.choice === 'yes', resolver: d.resolver, note: d.reason };
}

export type Settings = { ref?: ClassifierRef; judgeModel?: ModelRef; memo: string; clef: Limit; escalations: Limit };
export type Answered = { unit: Unit; check: Check; answer: Answer };

// One decision-model request per packed group; confident answers stand, the rest escalate concurrently within the escalation limit.
export async function answerItems(api: ToolExecutionApi, s: Settings, items: Item[], ctx: Context, onAnswer?: (a: Answered) => Promise<void>): Promise<Answered[]> {
  const packed = !!items[0].unit.shared;
  const key = items.length === 1 ? `${s.memo}:${items[0].unit.id}:classifier` : `${s.memo}:${items[0].unit.id}+${items.length}:classifier`;
  const stored = await api.memo<Record<string, number> | null>(key, ctx);
  const fresh = stored !== undefined ? stored : s.ref ? await classify(api, s.ref, items, s.clef, ctx) : null;
  const probabilities = stored !== undefined || fresh === undefined ? fresh : await api.memo(key, fresh, ctx);
  const act = s.ref?.act ?? 0.8;
  const jobs = items.flatMap((it, i) => it.checks.map(async (check) => {
    const p = probabilities?.[qid(check, i, packed)];
    const answer = p !== undefined && (p >= act || 1 - p >= act) ? { met: p >= act, resolver: s.ref!.modelId } : await s.escalations(() => escalate(api, s.judgeModel, s.memo, it.unit, check, ctx));
    const a = { unit: it.unit, check, answer };
    await onAnswer?.(a);
    return a;
  }));
  return Promise.all(jobs);
}
