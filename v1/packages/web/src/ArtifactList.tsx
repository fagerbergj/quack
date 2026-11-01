import { useEffect, useState } from 'react';
import { indexArtifacts, listArtifacts, type IndexArtifact, type SealedArtifact } from './api.ts';
import { age, titleOf, type Row } from './sessions.ts';
import { Work } from './Work.tsx';

const tone = (v: IndexArtifact['verdict']): [string, string] =>
  !v ? ['Not checked', 'bg-soft text-muted'] : v.passed ? ['Passed', 'bg-success-bg text-success'] : ['Failed', 'bg-danger-bg text-danger'];

function Card({ a, title, now, onOpen }: { a: IndexArtifact; title: string; now: number; onOpen: () => void }) {
  const [label, cls] = tone(a.verdict);
  return (
    <li>
      <button type="button" aria-label={`Open ${a.kind} v${a.version} of ${title}`} onClick={onOpen} className="flex min-h-11 w-full cursor-pointer flex-col gap-3 rounded-[22px] border border-line bg-surface p-5 text-left text-ink hover:border-line-strong hover:shadow-lift">
        <span className="flex items-center gap-2">
          <span className="rounded-md bg-soft px-2 py-0.5 text-xs text-ink-2">{a.kind}</span>
          <span className={`ml-auto rounded-full px-2 py-0.5 text-[11px] font-semibold ${cls}`}>{label}</span>
        </span>
        <span className="line-clamp-2 text-[15px] font-semibold leading-snug [overflow-wrap:anywhere]">{title}</span>
        <span className="flex justify-between gap-2 text-[13px] text-muted"><span>{`v${a.version}`}</span><time className="tabular-nums">{age(new Date(a.sealedAt).toISOString(), now)}</time></span>
      </button>
    </li>
  );
}

export function ArtifactList({ rows, now }: { rows: Row[] | undefined; now: number }) {
  const [items, setItems] = useState<IndexArtifact[] | null>();
  const [open, setOpen] = useState<SealedArtifact>();
  const [error, setError] = useState('');
  useEffect(() => void indexArtifacts().then((r) => setItems(r ?? null)), []);
  // The index holds no content, so opening reads the full version from its session.
  const show = (a: IndexArtifact) =>
    listArtifacts(a.session).then((all) => {
      const hit = all.find((x) => x.kind === a.kind && x.version === a.version);
      if (hit) setOpen(hit);
      else setError('That version is no longer in its session.');
    }, (e: Error) => setError(e.message));
  const titleFor = (key: string) => {
    const r = rows?.find((x) => x.key === key);
    return r ? titleOf(r) : key;
  };
  return (
    <div className="mx-auto w-full max-w-[1100px] px-4 pt-8 pb-10 sm:px-6">
      <h1 className="m-0 mb-5 text-[22px] font-bold tracking-[-0.02em]">Artifacts</h1>
      {error && <p role="alert" className="text-[13px] text-danger">{error}</p>}
      {items === null && <p className="text-sm text-muted">The index is off, so artifacts can only be opened from their sessions.</p>}
      {items?.length === 0 && <p className="text-sm text-muted">No sealed artifacts yet.</p>}
      <ul className="m-0 grid list-none grid-cols-[repeat(auto-fill,minmax(min(320px,100%),1fr))] gap-4 p-0">
        {items?.map((a) => <Card key={`${a.session}:${a.kind}:${a.version}`} a={a} title={titleFor(a.session)} now={now} onOpen={() => void show(a)} />)}
      </ul>
      {open && <Work artifact={open} onClose={() => setOpen(undefined)} />}
    </div>
  );
}
