import { escapeTerm } from '@quack/sdk/query';
import { useEffect, useState } from 'react';
import { listAllArtifacts, type Artifact, type SealedArtifact } from './api.ts';
import { artifactHref } from './extension.ts';
import { artifactName } from './Artifacts.tsx';
import { artifactKind } from './kinds.ts';
import { SearchFilter, searchTerms, type FilterField, type FilterValues } from './SearchFilter.tsx';
import { age, titleOf } from './sessions.ts';
import type { Row } from './sessions.ts';
import { useLoad } from './useLoad.ts';

function Item({ a, noun, title, now }: { a: Artifact; noun: string; title: string; now: number }) {
  return (
    <li>
      <a href={artifactHref(a.session, a.kind)} aria-label={`Open ${title}`} className="flex min-h-16 w-full flex-col justify-center gap-0.5 px-3 py-2 text-left text-ink no-underline hover:bg-soft sm:px-4">
        <span className="flex w-full items-baseline gap-2">
          <span className="min-w-0 flex-1 truncate text-[15px] font-semibold">{title}</span>
          <time className="flex-none text-[13px] tabular-nums text-muted">{age(a.sealedAt, now)}</time>
        </span>
        <span className="w-full truncate text-[13px] text-muted">{[noun, a.summary].filter(Boolean).join(' · ')}</span>
      </a>
    </li>
  );
}

// One row per session and kind, showing its newest version.
function newest(all: Artifact[]): Artifact[] {
  const groups = Map.groupBy(all, (x) => `${x.session}\n${x.kind}`);
  return [...groups.values()].map((g) => g.reduce((m, x) => (x.version > m.version ? x : m))).sort((x, y) => Date.parse(y.sealedAt) - Date.parse(x.sealedAt));
}

const SEALED: Record<string, { label: string; hours: number }> = { day: { label: 'Past day', hours: 24 }, week: { label: 'Past week', hours: 168 }, month: { label: 'Past 30 days', hours: 720 } };

/** The Artifacts list's q: search reads the summary or the session key; `sealed` starts on the hour, so q holds still between refreshes. */
export function artifactQuery(text: string, f: FilterValues, now: number): string {
  const since = SEALED[f.sealed ?? ''];
  const from = since && `${new Date(now - since.hours * 3_600_000).toISOString().slice(0, 13)}:00:00Z`;
  return [...(f.kind ? [`kind:${escapeTerm(f.kind)}`] : []), ...(from ? [`sealedAt:>=${escapeTerm(from)}`] : []), ...searchTerms(text, (w) => `session:*${w}*`)].join(' AND ');
}

// A kind no extension names reads as its words rather than as the generic "Result".
const kindLabel = (kind: string) => {
  const name = artifactName({ kind, content: '' } as SealedArtifact);
  return name === 'Result' ? kind.replace(/[._-]+/g, ' ') : name;
};
const fieldsOf = (kinds: string[]): FilterField[] => [
  { id: 'kind', label: 'Kind', any: 'Any kind', options: kinds.map((k) => ({ value: k, label: kindLabel(k) })) },
  { id: 'sealed', label: 'Sealed', any: 'Any time', options: Object.entries(SEALED).map(([value, s]) => ({ value, label: s.label })) },
];

export function ArtifactList({ rows, now }: { rows: Row[] | undefined; now: number }) {
  const [text, setText] = useState('');
  const [values, setValues] = useState<FilterValues>({});
  const q = artifactQuery(text, values, now);
  const { data: items, error: listError } = useLoad<Artifact[] | undefined>(() => listAllArtifacts(q), [q], undefined);
  // The kind menu remembers what the unfiltered list held, so choosing a kind keeps the others on offer.
  const [kinds, setKinds] = useState<string[]>([]);
  useEffect(() => void (items && !q && setKinds([...new Set(items.map((a) => a.kind))].sort())), [items]);
  const entries = (items ? newest(items) : []).map((a) => {
    const r = rows?.find((x) => x.key === a.session);
    return { a, r, ...artifactKind(r) };
  });
  return (
    <div className="mx-auto w-full max-w-[960px] px-4 pt-4 pb-10 sm:px-6 lg:pt-8">
      <h1 className="m-0 mb-4 hidden text-[22px] font-bold tracking-[-0.02em] lg:block">Artifacts</h1>
      <div className="mb-3">
        <SearchFilter label="Search artifacts" text={text} onText={setText} fields={fieldsOf(kinds)} values={values} onValues={setValues} />
      </div>
      {listError && <p role="alert" className="text-[13px] text-danger">{listError}</p>}
      {items?.length === 0 && <p className="text-sm text-muted">{q ? 'No artifacts match.' : 'Nothing here yet.'}</p>}
      <ul className="m-0 flex list-none flex-col divide-y divide-line overflow-hidden rounded-[22px] border border-line bg-surface p-0 shadow-card empty:hidden">
        {entries.map(({ a, r, noun }) => <Item key={`${a.session}:${a.kind}`} a={a} noun={noun} title={r ? titleOf(r) : a.session} now={now} />)}
      </ul>
    </div>
  );
}
