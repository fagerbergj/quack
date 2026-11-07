import { useState } from 'react';
import { listAllArtifacts, listArtifacts, type Artifact, type SealedArtifact } from './api.ts';
import { artifactKind } from './kinds.ts';
import { age, titleOf } from './sessions.ts';
import type { Row } from './sessions.ts';
import { useLoad } from './useLoad.ts';
import { Work } from './Work.tsx';

function Item({ a, noun, title, now, onOpen }: { a: Artifact; noun: string; title: string; now: number; onOpen: () => void }) {
  return (
    <li>
      <button type="button" aria-label={`Open ${title}`} onClick={onOpen} className="flex min-h-16 w-full cursor-pointer flex-col justify-center gap-0.5 px-3 py-2 text-left text-ink hover:bg-soft sm:px-4">
        <span className="flex w-full items-baseline gap-2">
          <span className="min-w-0 flex-1 truncate text-[15px] font-semibold">{title}</span>
          <time className="flex-none text-[13px] tabular-nums text-muted">{age(a.sealedAt, now)}</time>
        </span>
        <span className="w-full truncate text-[13px] text-muted">{[noun, a.summary].filter(Boolean).join(' · ')}</span>
      </button>
    </li>
  );
}

// One row per session and kind, showing its newest version.
function newest(all: Artifact[]): Artifact[] {
  const groups = Map.groupBy(all, (x) => `${x.session}\n${x.kind}`);
  return [...groups.values()].map((g) => g.reduce((m, x) => (x.version > m.version ? x : m))).sort((x, y) => Date.parse(y.sealedAt) - Date.parse(x.sealedAt));
}

export function ArtifactList({ rows, now }: { rows: Row[] | undefined; now: number }) {
  const { data: items, error: listError } = useLoad<Artifact[] | undefined>(listAllArtifacts, [], undefined);
  const [open, setOpen] = useState<SealedArtifact[]>();
  const [session, setSession] = useState<string>();
  const [error, setError] = useState('');
  const [group, setGroup] = useState('');
  // The list carries no content, so opening reads the full versions from their session.
  const show = (a: Artifact) =>
    listArtifacts(a.session).then((all) => {
      const versions = all.filter((x) => x.kind === a.kind).sort((x, y) => y.version - x.version);
      if (versions.length) {
        setSession(a.session);
        setOpen(versions);
      }
      else setError('That artifact is no longer in its session.');
    }, (e: Error) => setError(e.message));
  const entries = (items ? newest(items) : []).map((a) => {
    const r = rows?.find((x) => x.key === a.session);
    return { a, r, ...artifactKind(r) };
  });
  const groups = [...new Set(entries.map((x) => x.group))];
  return (
    <div className="mx-auto w-full max-w-[960px] px-4 pt-4 pb-10 sm:px-6 lg:pt-8">
      <h1 className="m-0 mb-4 hidden text-[22px] font-bold tracking-[-0.02em] lg:block">Artifacts</h1>
      {groups.length > 1 && (
        <div role="group" aria-label="Filter by kind" className="mb-3 flex flex-wrap gap-2">
          {groups.map((g) => <button key={g} type="button" aria-pressed={group === g} onClick={() => setGroup(group === g ? '' : g)} className="min-h-11 cursor-pointer rounded-full border border-line px-4 text-[13px] text-ink-2 aria-pressed:border-link aria-pressed:bg-link-bg aria-pressed:text-link">{g}</button>)}
        </div>
      )}
      {(error || listError) && <p role="alert" className="text-[13px] text-danger">{error || listError}</p>}
      {items?.length === 0 && <p className="text-sm text-muted">Nothing here yet.</p>}
      <ul className="m-0 flex list-none flex-col divide-y divide-line overflow-hidden rounded-[22px] border border-line bg-surface p-0 shadow-card empty:hidden">
        {entries.filter((x) => !group || x.group === group).map(({ a, r, noun }) => <Item key={`${a.session}:${a.kind}`} a={a} noun={noun} title={r ? titleOf(r) : a.session} now={now} onOpen={() => void show(a)} />)}
      </ul>
      {open && <Work artifact={open[0]!} versions={open} session={session} onClose={() => setOpen(undefined)} />}
    </div>
  );
}
