import { useEffect, useState } from 'react';
import { indexArtifacts, listArtifacts, type IndexArtifact, type SealedArtifact } from './api.ts';
import { artifactKind } from './kinds.ts';
import { age, titleOf } from './sessions.ts';
import type { Row } from './sessions.ts';
import { Work } from './Work.tsx';

type Status = { label: string; cls: string; path: string };
const MARKS = {
  ok: { label: 'Passed', cls: 'text-success', path: 'M5 12.5l4.5 4.5L19 7.5' },
  revising: { label: 'Revising', cls: 'text-muted', path: 'M12 6v6l4 2M21 12a9 9 0 1 1-18 0a9 9 0 0 1 18 0' },
  bad: { label: "Didn't pass", cls: 'text-danger', path: 'M6 6l12 12M18 6L6 18' },
  none: { label: 'Not checked', cls: 'text-muted', path: 'M9 12a3 3 0 1 0 6 0a3 3 0 1 0-6 0' },
} satisfies Record<string, Status>;

// A failed newest version is only a failure once the session has stopped; while it runs, a revision is coming.
export const statusOf = (v: IndexArtifact['verdict'], active: boolean): Status => (!v ? MARKS.none : v.passed ? MARKS.ok : active ? MARKS.revising : MARKS.bad);

function Item({ a, noun, title, active, now, onOpen }: { a: IndexArtifact; noun: string; title: string; active: boolean; now: number; onOpen: () => void }) {
  const st = statusOf(a.verdict, active);
  return (
    <li>
      <button type="button" aria-label={`Open ${title}`} onClick={onOpen} className="flex min-h-16 w-full cursor-pointer items-center gap-3 px-3 py-2 text-left text-ink hover:bg-soft sm:px-4">
        <svg role="img" aria-label={st.label} width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" className={`flex-none ${st.cls}`}><path d={st.path} /></svg>
        <span className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span className="flex items-baseline gap-2">
            <span className="min-w-0 flex-1 truncate text-[15px] font-semibold">{title}</span>
            <time className="flex-none text-[13px] tabular-nums text-muted">{age(new Date(a.sealedAt).toISOString(), now)}</time>
          </span>
          <span className="truncate text-[13px] text-muted">{[noun, a.summary].filter(Boolean).join(' · ')}</span>
        </span>
      </button>
    </li>
  );
}

// One row per session and kind, showing its newest version.
function newest(all: IndexArtifact[]): IndexArtifact[] {
  const groups = Map.groupBy(all, (x) => `${x.session}\n${x.kind}`);
  return [...groups.values()].map((g) => g.reduce((m, x) => (x.version > m.version ? x : m))).sort((x, y) => y.sealedAt - x.sealedAt);
}

export function ArtifactList({ rows, now }: { rows: Row[] | undefined; now: number }) {
  const [items, setItems] = useState<IndexArtifact[] | null>();
  const [open, setOpen] = useState<SealedArtifact[]>();
  const [session, setSession] = useState<string>();
  const [error, setError] = useState('');
  const [group, setGroup] = useState('');
  useEffect(() => void indexArtifacts().then((r) => setItems(r ?? null)), [now]);
  // The index holds no content, so opening reads the full version from its session.
  const show = (a: IndexArtifact) =>
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
      {error && <p role="alert" className="text-[13px] text-danger">{error}</p>}
      {items === null && <p className="text-sm text-muted">The index is off, so artifacts can only be opened from their sessions.</p>}
      {items?.length === 0 && <p className="text-sm text-muted">Nothing here yet.</p>}
      <ul className="m-0 flex list-none flex-col divide-y divide-line overflow-hidden rounded-[22px] border border-line bg-surface p-0 shadow-card empty:hidden">
        {entries.filter((x) => !group || x.group === group).map(({ a, r, noun }) => <Item key={`${a.session}:${a.kind}`} a={a} noun={noun} title={r ? titleOf(r) : a.session} active={r?.active ?? false} now={now} onOpen={() => void show(a)} />)}
      </ul>
      {open && <Work artifact={open[0]!} versions={open} session={session} onClose={() => setOpen(undefined)} />}
    </div>
  );
}
