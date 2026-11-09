import { listArtifacts, type SealedArtifact } from './api.ts';
import { ArtifactView, paneVersions } from './ArtifactPane.tsx';
import { artifactName } from './Artifacts.tsx';
import { versionOf } from './delivered.ts';
import { artifactHref } from './extension.ts';
import { sessionTitle } from './kinds.ts';
import type { Row } from './sessions.ts';
import { useLoad } from './useLoad.ts';

/** `#/s/<key>/a/<kind>`, with `?v=<version>` to pick one; undefined for any other route. */
export function parseArtifactRoute(hash: string): { key: string; kind: string; version?: number } | undefined {
  const m = /^#\/s\/([^/?]+)\/a\/([^/?]+)(?:\?v=(\d+))?$/.exec(hash);
  try {
    return m ? { key: decodeURIComponent(m[1]!), kind: decodeURIComponent(m[2]!), version: m[3] ? Number(m[3]) : undefined } : undefined;
  } catch {
    return undefined;
  }
}

const iconBtn = 'grid h-11 w-11 flex-none place-items-center rounded-full text-ink-2 no-underline hover:bg-soft hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link';
const BACK = 'M15 5l-7 7 7 7';
const CHAT = 'M5 5h14a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-7l-5 4v-4H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2z';

function IconLink({ href, label, d, className = '' }: { href: string; label: string; d: string; className?: string }) {
  return (
    <a href={href} aria-label={label} title={label} className={`${iconBtn} ${className}`}>
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={d} /></svg>
    </a>
  );
}

/** One artifact on its own page, opened from the Artifacts list: its versions as in the session pane, and a link to the session it came from. */
export function ArtifactPage({ sessionKey, kind, version, rows }: { sessionKey: string; kind: string; version?: number; rows: Row[] | undefined }) {
  const { data: items, error } = useLoad(async (): Promise<SealedArtifact[] | undefined> => (await listArtifacts(sessionKey)).filter((a) => a.kind === kind), [sessionKey, kind], undefined);
  const shown = items && paneVersions(items, version === undefined ? undefined : items.find((a) => versionOf(a) === version));
  const row = rows?.find((r) => r.key === sessionKey);
  const t = sessionTitle(row ?? { key: sessionKey, agent: '' });
  const session = [t.number, t.full].filter(Boolean).join(' ');
  // A picked version replaces the entry, so Back returns to where the page was opened from.
  const pick = (a: SealedArtifact) => location.replace(artifactHref(sessionKey, kind, versionOf(a)));
  return (
    <div>
      <div className="sticky top-0 z-10 flex min-h-[calc(3.5rem+env(safe-area-inset-top,0px))] items-center gap-0.5 border-b border-line bg-canvas/85 px-1 pt-[env(safe-area-inset-top,0px)] backdrop-blur-md lg:gap-1 lg:px-3">
        <IconLink href="#/artifacts" label="Artifacts" d={BACK} className="lg:hidden" />
        <div className="flex min-w-0 flex-1 flex-col justify-center px-1">
          <h1 className="m-0 truncate font-sans text-[15px] leading-5 font-semibold">{shown ? artifactName(shown.a) : 'Artifact'}</h1>
          <p className="m-0 truncate text-[12px] leading-[18px] text-muted" title={session}>{session}</p>
        </div>
        <IconLink href={`#/s/${encodeURIComponent(sessionKey)}`} label="Open its session" d={CHAT} />
      </div>
      {error && <p role="alert" className="mx-4 text-[13px] text-danger">{error}</p>}
      {items && !shown && <p className="mx-4 text-sm text-muted">That artifact is no longer in its session.</p>}
      {shown && <ArtifactView sessionKey={sessionKey} {...shown} onPick={pick} />}
    </div>
  );
}
