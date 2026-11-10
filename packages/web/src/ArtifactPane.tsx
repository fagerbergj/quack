import type { SealedArtifact, Verdict } from './api.ts';
import { ArtifactBoundary } from './ArtifactBoundary.tsx';
import { artifactCard, artifactName } from './Artifacts.tsx';
import { CheckResult } from './CheckResult.tsx';
import { delivered, versionOf } from './delivered.ts';
import { SessionKeyContext } from './extension.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { Content } from './JsonView.tsx';
import { FollowNewest, VersionMenu, type VersionItem } from './VersionMenu.tsx';

/** The versions the pane offers and the one it shows: the focused artifact's kind, else the kind sealed last; the focused version, else the newest. */
export function paneVersions(items: SealedArtifact[], focus?: SealedArtifact): { versions: SealedArtifact[]; a: SealedArtifact } | undefined {
  if (!items.length) return undefined;
  const kind = focus?.kind ?? items.reduce((n, a) => (a.sealedAt >= n.sealedAt ? a : n)).kind;
  const versions = delivered(items.filter((a) => a.kind === kind));
  return { versions, a: (focus && versions.find((v) => versionOf(v) === versionOf(focus))) ?? versions[0]! };
}

// Over REST a version has no verdict at all, so it gets no status; null means its checks are still running.
const statusOf = (v: Verdict | null | undefined): Pick<VersionItem, 'status' | 'badge'> => {
  if (v === undefined) return {};
  if (v === null) return { status: 'checking', badge: { text: 'checking', cls: 'bg-soft text-muted' } };
  return v.passed ? { status: 'passed', badge: { text: 'passed', cls: 'bg-success-bg text-success' } } : { status: "didn't pass", badge: { text: 'failed', cls: 'bg-danger-bg text-danger' } };
};
const itemsOf = (versions: SealedArtifact[]): VersionItem[] =>
  versions.map((v) => ({ id: versionOf(v), short: `v${versionOf(v)}`, name: `Version ${versionOf(v)}`, ...statusOf(v.verdict) })).reverse();

const fullOf = (a: SealedArtifact) => EXTENSIONS.map((e) => e.full?.(a)).find(Boolean);

function Body({ a, now }: { a: SealedArtifact; now?: { running: boolean; meta: readonly Record<string, unknown>[] } }) {
  const full = fullOf(a);
  if (full) return <ArtifactBoundary artifact={a}>{full}</ArtifactBoundary>;
  return artifactCard(a, now) ?? (
    <div className="rounded-[22px] border border-line bg-surface p-5 text-sm leading-relaxed text-ink-2 shadow-card [overflow-wrap:anywhere]">
      <p className="m-0 mb-3"><span className="rounded-md bg-soft px-2 py-0.5 text-xs text-ink-2">{artifactName(a)}</span></p>
      <Content text={a.content} />
    </div>
  );
}

export const paneLayout = 'mx-auto flex w-full max-w-[1100px] min-w-0 flex-col gap-4 px-4 pt-4 pb-10 sm:px-5';

/** One artifact with its version row, as the session pane and the artifact's own page both draw it; `now` is the live run, for the newest version. */
export function ArtifactView({ sessionKey, versions, a, onPick, onFollow, now }: { sessionKey: string; versions: SealedArtifact[]; a: SealedArtifact; onPick: (v: SealedArtifact) => void; onFollow?: () => void; now?: { running: boolean; meta: readonly Record<string, unknown>[] } }) {
  const pick = (n?: number) => { const v = versions.find((x) => versionOf(x) === n); if (v) onPick(v); };
  return (
    <div className={paneLayout}>
      <div className="flex items-center gap-1.5">
        <VersionMenu name="Version" items={itemsOf(versions)} value={versionOf(a)} onPick={pick} />
        <span className="flex-1" />
        {onFollow && <FollowNewest onClick={onFollow} />}
      </div>
      <SessionKeyContext value={sessionKey}><Body a={a} now={a === versions[0] ? now : undefined} /></SessionKeyContext>
      {a.verdict && <CheckResult verdict={a.verdict} open={!a.verdict.passed} />}
    </div>
  );
}
