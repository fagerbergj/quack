import { useCallback } from 'react';
import type { Approval, SealedArtifact } from './api.ts';
import type { Marking, StandOf } from './extension.ts';
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

export type MarkOf = (a: SealedArtifact) => Marking;
/** A version's mark from its verdict alone, for kinds no extension stands for. */
export const byVerdict: MarkOf = (a) => verdictMark(a.verdict);
const NO_STANDING = new Map<string, StandOf>();

/** The session's one read of where its versions stand with the person (a contract's approval), by kind; `stand` folds in the open approvals as
 * they arrive, and `markOf` falls back to the verdict. Read again when a version is sealed, an approval opens or closes, or a run starts or ends. */
export function useStanding(sessionKey: string, items: SealedArtifact[], approvals: Approval[], running: boolean) {
  const kinds = [...new Set(items.map((a) => a.kind))];
  // The approvals come as a new array every render; only which are open re-reads and re-marks, so transcript rows stay put.
  const open = approvals.map((p) => p.id).join();
  const { data } = useLoad(async () => {
    const read = await Promise.all(kinds.flatMap((k) => EXTENSIONS.map((e) => e.standing?.(sessionKey, k)?.then((f) => (f ? [[k, f] as const] : [])) ?? [])));
    const owned = read.flat();
    return owned.length ? new Map(owned) : NO_STANDING;
  }, [sessionKey, kinds.join(), items.length, open, running], NO_STANDING);
  const stand = useCallback((a: SealedArtifact) => data.get(a.kind)?.(a, approvals), [data, open]);
  const markOf = useCallback<MarkOf>((a) => stand(a) ?? byVerdict(a), [stand]);
  return { stand, markOf };
}
