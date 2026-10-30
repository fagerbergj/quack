import type { Context } from '@earendil-works/chord/context';
import type { ClassifierModel, ClassifierQuestion } from '@earendil-works/pi-ai';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { agentModel, decide } from '@quack/sdk';
import type { Check, Unit } from './rubric.ts';

/** A decision model such as Clef, reached through pi-ai's classifier API. */
export type ClassifierRef = { provider: string; modelId: string; maxInputTokens: number; act?: number };

export type Answer = { met: boolean | null; resolver: string; note?: string };

const BYTES_PER_TOKEN = 4;

async function classify(api: ToolExecutionApi, ref: ClassifierRef, unit: Unit, checks: Check[], ctx: Context): Promise<Record<string, number> | undefined> {
  const questions: Record<string, ClassifierQuestion> = Object.fromEntries(
    checks.map((c) => [c.id, { type: 'bool', instructions: c.question!, criteria: { true: 'Yes, this holds.', false: 'No, it does not hold.' } }]),
  );
  // The decision model has a hard input cap; over it, every question escalates instead of being truncated.
  if (JSON.stringify({ state: unit.state, questions }).length / BYTES_PER_TOKEN > ref.maxInputTokens) return undefined;
  const model = api.models.getAllModels(ref.provider).find((m) => m.id === ref.modelId && m.type === 'classifier') as ClassifierModel<never> | undefined;
  if (!model) return undefined;
  try {
    const result = await api.models.classify(model, { state: unit.state, questions }, { signal: ctx.abortSignal });
    if (result.stopReason !== 'stop') return undefined;
    return Object.fromEntries(Object.entries(result.answers).flatMap(([id, a]) => (a.type === 'bool' ? [[id, a.probability]] : [])));
  } catch {
    return undefined;
  }
}

async function escalate(api: ToolExecutionApi, memo: string, unit: Unit, check: Check, ctx: Context): Promise<Answer> {
  const point = { name: `${memo}:${unit.id}:${check.id}`, question: check.question!, options: ['yes', 'no'], evidence: JSON.stringify(unit.state, null, 1) };
  const d = await decide(api, point, [agentModel], ctx);
  if (!d) return { met: null, resolver: 'none', note: 'no resolver could decide' };
  return { met: d.choice === 'yes', resolver: d.resolver, note: d.choice === 'yes' ? undefined : d.reason };
}

// One decision-model request per unit; confident answers stand, the rest escalate one check at a time (D16).
export async function answerUnit(api: ToolExecutionApi, ref: ClassifierRef | undefined, memo: string, unit: Unit, checks: Check[], ctx: Context): Promise<Record<string, Answer>> {
  const key = `${memo}:${unit.id}:classifier`;
  const stored = await api.memo<Record<string, number> | null>(key, ctx);
  const probabilities = stored !== undefined ? stored : await api.memo(key, (ref ? await classify(api, ref, unit, checks, ctx) : undefined) ?? null, ctx);
  const act = ref?.act ?? 0.8;
  const answers: Record<string, Answer> = {};
  for (const check of checks) {
    const p = probabilities?.[check.id];
    if (p !== undefined && (p >= act || p <= 1 - act)) answers[check.id] = { met: p >= act, resolver: ref!.modelId };
    else answers[check.id] = await escalate(api, memo, unit, check, ctx);
  }
  return answers;
}
