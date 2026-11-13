import type { EvalAdapter, ExtensionManifest } from '@quack/sdk';
import { buildEvidence, codeResults, score, type Declaration, type Evidence, type Rubric } from '@quack/judge';
import { asJudged, type SessionRecord } from './record.ts';
import type { EvalServices } from './replay.ts';
import type { Blocks } from './report.ts';

// An older ref may lack an extension this tree lists (its module exports no manifest yet); it contributes no adapters.
export const adaptersOf = (manifests: Array<ExtensionManifest | undefined>): EvalAdapter[] => manifests.flatMap((m) => m?.evalAdapters ?? []);

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
    const judged = asJudged(record, a.kind);
    // Without a model a declared scope split by the LLM splitter has no items; code checks read the deterministic ones.
    // An adapter whose inputs throw is declining the recording (it lacks what they are rebuilt from), as in judge replay.
    const inputs = a.judge.declaration ? await a.judge.inputs!(judged, content).catch(() => undefined) : undefined;
    const evidence = !a.judge.declaration
      ? ((await a.judge.evidence!(judged, content)) as Evidence | undefined)
      : inputs && (await buildEvidence(a.judge.declaration as Declaration, inputs)).evidence;
    if (!evidence) continue;
    const s = score(rubric, codeResults(rubric, content, evidence));
    out[`${a.kind}.code_score`] = s.score;
    out[`${a.kind}.code_gates_failed`] = s.gatesFailed.length;
  }
  return out;
}
