import type { ClassifierModel } from '@earendil-works/pi-ai';
import type { ClassifierRef } from '@quack/judge';
import type { Resolver } from '@quack/sdk';

const BYTES_PER_TOKEN = 4;

/** A decision model such as Clef picking one option; undefined (escalate) when it is missing, over its input cap, or fails. */
export function classifierChoice(ref: ClassifierRef, describe: (option: string) => string): Resolver {
  return {
    id: ref.modelId,
    async resolve(point, api, ctx) {
      const state = { evidence: point.evidence ?? '' };
      const questions = { pick: { type: 'choice' as const, instructions: point.question, criteria: Object.fromEntries(point.options.map((o) => [o, describe(o)])) } };
      if (JSON.stringify({ state, questions }).length / BYTES_PER_TOKEN > ref.maxInputTokens) return undefined;
      const model = api.models.getAllModels(ref.provider).find((m) => m.id === ref.modelId && m.type === 'classifier') as ClassifierModel<never> | undefined;
      if (!model) return undefined;
      try {
        const result = await api.models.classify(model, { state, questions }, { signal: ctx.abortSignal });
        const a = result.stopReason === 'stop' ? result.answers.pick : undefined;
        return a?.type === 'choice' && point.options.includes(a.choice) ? { choice: a.choice, confidence: a.confidence, reason: `${ref.modelId} confidence ${a.confidence.toFixed(2)}` } : undefined;
      } catch {
        return undefined;
      }
    },
  };
}

/** Reads a model's one-line topic proposal; throws when it is not a 1 to 4 word name. An existing topic differing only in case wins. */
export function parseTopic(text: string, existing: string[]): string {
  const name = (text.trim().split('\n')[0] ?? '').replace(/^["'`*\s]+|["'`*.\s]+$/g, '').replace(/\s+/g, ' ');
  if (!name || name.length > 40 || name.split(' ').length > 4) throw new Error('the model did not propose a short topic name');
  return existing.find((t) => t.toLowerCase() === name.toLowerCase()) ?? name;
}
