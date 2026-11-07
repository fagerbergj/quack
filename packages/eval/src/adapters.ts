import type { EvalAdapter, ExtensionManifest } from '@quack/sdk';
import { codeResults, score, type Evidence, type Rubric } from '@quack/judge';
import { asJudged, type SessionRecord } from './record.ts';
import type { EvalServices } from './replay.ts';
import type { Blocks } from './report.ts';

export const adaptersOf = (manifests: ExtensionManifest[]): EvalAdapter[] => manifests.flatMap((m) => m.evalAdapters ?? []);

/** The service stubs every adapter contributes, by the pinned document kind they serve. */
export const servicesOf = (adapters: EvalAdapter[]): EvalServices => Object.assign({}, ...adapters.map((a) => a.services));

export const blocksOf = (adapters: EvalAdapter[]): Blocks => (finals) => {
  for (const a of adapters) {
    const verdict = finals[a.kind] === undefined ? undefined : a.blocks?.(finals[a.kind]);
    if (verdict !== undefined) return verdict;
  }
  return undefined;
};

/** Each adapter's rubric applied to its kind's final artifact with the rubric's code checks; model checks need a model and are left out. */
export async function adapterScores(adapters: EvalAdapter[], record: SessionRecord): Promise<Record<string, number>> {
  const out: Record<string, number> = {};
  for (const a of adapters) {
    const content = record.artifacts[a.kind]?.at(-1)?.content;
    if (!a.judge || content === undefined) continue;
    const rubric = a.judge.rubric as Rubric;
    const evidence = (await a.judge.evidence(asJudged(record, a.kind), content)) as Evidence | undefined;
    if (!evidence) continue;
    const s = score(rubric, codeResults(rubric, content, evidence));
    out[`${a.kind}.code_score`] = s.score;
    out[`${a.kind}.code_gates_failed`] = s.gatesFailed.length;
  }
  return out;
}
