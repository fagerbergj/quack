import { useCallback } from 'react';
import type { SealedArtifact } from './api.ts';
import type { Standing } from './extension.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { useLoad } from './useLoad.ts';
import { verdictMark } from './VersionMenu.tsx';

/** The version a person knows an artifact by: its content's own, else the sealed one. */
export const versionOf = (a: SealedArtifact): number => EXTENSIONS.map((e) => e.versionOf?.(a)).find((v) => v !== undefined) ?? a.version;

/** Rows listed over the Sessions API, labelled by whichever extension owns their kind. */
export async function labelled(sessionKey: string, kind: string, rows: SealedArtifact[]): Promise<SealedArtifact[]> {
  let out = rows;
  for (const e of EXTENSIONS) out = (await e.label?.(sessionKey, kind, out)) ?? out;
  return out;
}

// A draft the judge sends back is sealed again under the same version, so each version shows its newest delivered seal (passed, or released at the revision limit), else its newest (still judging).
export function delivered(all: SealedArtifact[]): SealedArtifact[] {
  const groups = Map.groupBy([...all].sort((a, b) => b.version - a.version), versionOf);
  return [...groups.entries()].sort((x, y) => y[0] - x[0]).map(([, g]) => g.find((a) => a.verdict?.passed || a.verdict?.limitReached) ?? g[0]!);
}

export type MarkOf = (a: SealedArtifact) => Standing;
const NO_STANDING = new Map<string, Standing>();

/** Each version's mark and words: the owning extension's standing with the person where it has one (a contract's approval), else its verdict.
 * Read again when a version is sealed or `tick` changes (a proposal opening or being decided). */
export function useMarks(sessionKey: string, items: SealedArtifact[], tick: unknown): MarkOf {
  const kinds = [...new Set(items.map((a) => a.kind))];
  const { data } = useLoad(async () => {
    const read = await Promise.all(kinds.flatMap((k) => EXTENSIONS.map((e) => e.standing?.(sessionKey, k)?.then((m) => [...m].map(([n, s]) => [`${k}:${n}`, s] as const)) ?? [])));
    return new Map(read.flat());
  }, [sessionKey, kinds.join(), items.length, tick], NO_STANDING);
  return useCallback((a: SealedArtifact) => data.get(`${a.kind}:${a.version}`) ?? verdictMark(a.verdict), [data]);
}
