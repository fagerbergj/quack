import type { Context } from '@earendil-works/chord/context';
import type { ClassifierModel, ClassifierQuestion } from '@earendil-works/pi-ai';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { agentModel, decide, modelResolver } from '@quack/sdk';
import type { Check, Unit } from './rubric.ts';

/** A decision model such as Clef, reached through pi-ai's classifier API. */
export type ClassifierRef = { provider: string; modelId: string; maxInputTokens: number; act?: number };

export type Answer = { met: boolean | null; resolver: string; note?: string };

// Clef's tokenizer counted 9.6-10.6k tokens on code units the 4-byte rule put under 8,192 (spike #2190); code packs ~3 bytes a token.
const BYTES_PER_TOKEN = 3;

// null is a deterministic miss (over the cap, no such model) and is memoized; undefined is an outage, retried on a rerun.
async function classify(api: ToolExecutionApi, ref: ClassifierRef, unit: Unit, checks: Check[], ctx: Context): Promise<Record<string, number> | null | undefined> {
  const questions: Record<string, ClassifierQuestion> = Object.fromEntries(
    checks.map((c) => [c.id, { type: 'bool', instructions: c.question!, criteria: { true: 'Yes, this holds.', false: 'No, it does not hold.' } }]),
  );
  // The decision model has a hard input cap; over it, every question escalates instead of being truncated.
  if (JSON.stringify({ state: unit.state, questions }).length / BYTES_PER_TOKEN > ref.maxInputTokens) return null;
  const model = api.models.getAllModels(ref.provider).find((m) => m.id === ref.modelId && m.type === 'classifier') as ClassifierModel<never> | undefined;
  if (!model) return null;
  const result = await api.models.classify(model, { state: unit.state, questions }, { signal: ctx.abortSignal }).catch(() => undefined);
  ctx.abortSignal?.throwIfAborted();
  if (result?.stopReason !== 'stop') return undefined;
  return Object.fromEntries(Object.entries(result.answers).flatMap(([id, a]) => (a.type === 'bool' ? [[id, a.probability]] : [])));
}

/** Where uncertain checks go: a named judge model, or the agent's own model when none is set. */
export type ModelRef = { provider: string; modelId: string };

async function escalate(api: ToolExecutionApi, judgeModel: ModelRef | undefined, memo: string, unit: Unit, check: Check, ctx: Context): Promise<Answer> {
  const point = { name: `${memo}:${unit.id}:${check.id}`, question: check.question!, options: ['yes', 'no'], evidence: JSON.stringify(unit.state, null, 1) };
  const resolver = judgeModel ? { ...modelResolver(judgeModel.provider, judgeModel.modelId), id: 'judge-model' } : agentModel;
  const d = await decide(api, point, [resolver], ctx);
  if (!d) return { met: null, resolver: 'none', note: 'no resolver could decide' };
  return { met: d.choice === 'yes', resolver: d.resolver, note: d.reason };
}

// One decision-model request per unit; confident answers stand, the rest escalate one check at a time.
export async function answerUnit(api: ToolExecutionApi, ref: ClassifierRef | undefined, judgeModel: ModelRef | undefined, memo: string, unit: Unit, checks: Check[], ctx: Context): Promise<Record<string, Answer>> {
  const key = `${memo}:${unit.id}:classifier`;
  const stored = await api.memo<Record<string, number> | null>(key, ctx);
  const fresh = stored !== undefined ? stored : ref ? await classify(api, ref, unit, checks, ctx) : null;
  const probabilities = stored !== undefined || fresh === undefined ? fresh : await api.memo(key, fresh, ctx);
  const act = ref?.act ?? 0.8;
  const answers: Record<string, Answer> = {};
  for (const check of checks) {
    const p = probabilities?.[check.id];
    if (p !== undefined && (p >= act || 1 - p >= act)) answers[check.id] = { met: p >= act, resolver: ref!.modelId };
    else answers[check.id] = await escalate(api, judgeModel, memo, unit, check, ctx);
  }
  return answers;
}
