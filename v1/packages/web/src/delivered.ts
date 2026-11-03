import type { SealedArtifact } from './api.ts';
import { EXTENSIONS } from './extensions/index.ts';

/** The version a person knows an artifact by: its content's own, else the sealed one. */
export const versionOf = (a: SealedArtifact): number => EXTENSIONS.map((e) => e.versionOf?.(a)).find((v) => v !== undefined) ?? a.version;

// A draft the judge sends back is sealed again under the same version, so each version shows its newest delivered seal (passed, or released at the revision limit), else its newest (still judging).
export function delivered(all: SealedArtifact[]): SealedArtifact[] {
  const groups = Map.groupBy([...all].sort((a, b) => b.version - a.version), versionOf);
  return [...groups.entries()].sort((x, y) => y[0] - x[0]).map(([, g]) => g.find((a) => a.verdict?.passed || a.verdict?.limitReached) ?? g[0]!);
}
