import type { Context } from '@earendil-works/chord/context';
import { defineDoc, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { createHash } from 'node:crypto';
import { PROMPT_VERSION, type Answer, type Ask, type DecisionModelRef, type ModelRef } from './resolve.ts';
import type { Unit } from './rubric.ts';

/** A decided item answer: who gave it, its reason, and the version it was first judged on. */
export type Kept = { met: boolean; resolver: string; note?: string; version: number };

/** The session's decided item answers by reuse key: one entry per key, never dropped, so it is bounded by the items the session judged. */
export const ReuseDoc = defineDoc<{ answers: Record<string, Kept> }>({
  kind: 'judge.reuse', version: 1, scope: 'conversation', history: 'latest', fork: 'initial', initial: () => ({ answers: {} }), checkpointWhen: () => false,
});

/** Where an answer may come from: the decision model as configured, and the model escalations reach. */
export type Policy = { decisionModel?: DecisionModelRef; judgeModel?: ModelRef };

/** The check's declared question and pass, the unit's id and evidence (state and shared context), the resolver policy and the prompt version; not the rubric version. */
export function reuseKey(ask: Ask, unit: Unit, policy: Policy, prompts = PROMPT_VERSION): string {
  const d = policy.decisionModel;
  const j = policy.judgeModel;
  const parts = [prompts, ask.id, ask.question, ask.pass ?? null, unit.id, unit.shared ?? null, unit.state, d ? [d.provider, d.modelId, d.act ?? null, d.maxInputTokens] : null, ask.decisionModel ?? null, j ? [j.provider, j.modelId] : null];
  return createHash('sha256').update(JSON.stringify(parts)).digest('hex');
}

/** One judging round's view of the store: the answers decided before it, and a write per item it decides. */
export type Reuse = { answers: Record<string, Kept>; key: (ask: Ask, unit: Unit) => string; keep: (ask: Ask, unit: Unit, a: Answer) => Promise<void> };

export async function sessionReuse(api: ToolExecutionApi, policy: Policy, version: number, ctx: Context): Promise<Reuse> {
  const resolved = { ...policy, judgeModel: policy.judgeModel ?? (await api.agent?.(ctx))?.model };
  const answers = (await api.snapshot(ReuseDoc, api.conversationId, ctx))?.answers ?? {};
  const key = (ask: Ask, unit: Unit) => reuseKey(ask, unit, resolved);
  const keep = (ask: Ask, unit: Unit, a: Answer) => api.commit(async (tx) => {
    const doc = await tx.doc(ReuseDoc, api.conversationId);
    // The first decided answer is frozen: a later one for the same key never replaces it.
    doc.answers[key(ask, unit)] ??= { met: a.met!, resolver: a.resolver, version, ...(a.note === undefined ? {} : { note: a.note }) };
  }, ctx);
  return { answers, key, keep };
}
