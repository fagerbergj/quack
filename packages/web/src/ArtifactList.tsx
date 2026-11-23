import { escapeTerm } from '@quack/sdk/query';
import { useEffect, useState, type ReactNode } from 'react';
import { listAllArtifacts, type Artifact, type SealedArtifact } from './api.ts';
import { artifactHref, type ListRow } from './extension.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { artifactName } from './Artifacts.tsx';
import { sessionTitle } from './kinds.ts';
import { SearchFilter, searchTerms, type FilterField, type FilterValues } from './SearchFilter.tsx';
import { StatusMark } from './StatusIcon.tsx';
import type { Row } from './sessions.ts';
import { useLoad } from './useLoad.ts';

/** One session on the list: its newest artifact of each kind (newest first), named and drawn by its extension. */
type Entry = { key: string; latest: Artifact[]; at: number; title: string; icon: string; tint: string; row: ListRow };
type Item = { entry: Entry } | { fold: string; entries: Entry[] };

const CLOCK = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' });
const DATE = new Intl.DateTimeFormat(undefined, { weekday: 'short', month: 'short', day: 'numeric' });
const midnight = (t: number) => new Date(t).setHours(0, 0, 0, 0);

/** A day's heading in local time: Today, Yesterday, then "Fri, Nov 20". */
export function dayLabel(at: number, now: number): string {
  const days = Math.round((midnight(now) - midnight(at)) / 86_400_000);
  return days === 0 ? 'Today' : days === 1 ? 'Yesterday' : DATE.format(at);
}

const listed = (latest: Artifact[]): ListRow => EXTENSIONS.map((e) => e.listRow?.(latest)).find(Boolean) ?? {};
const NO_ROWS: ReadonlyMap<string, ListRow> = new Map();
const loadRows = async (rows: Row[]) => new Map((await Promise.all(EXTENSIONS.flatMap((e) => e.listRows?.(rows) ?? []))).flatMap((m) => [...m]));

// One entry per session, newest fact first.
function entriesOf(all: Artifact[], rows: Row[] | undefined, loaded: ReadonlyMap<string, ListRow>): Entry[] {
  const at = (a: Artifact) => Date.parse(a.sealedAt);
  return [...Map.groupBy(all, (a) => a.session)].map(([key, own]) => {
    const latest = [...Map.groupBy(own, (a) => a.kind).values()].map((g) => g.reduce((m, x) => (x.version > m.version ? x : m))).sort((x, y) => at(y) - at(x));
    const r = rows?.find((x) => x.key === key);
    const t = sessionTitle({ key, agent: r?.agent ?? '', title: r?.title });
    return { key, latest, at: at(latest[0]!), title: t.full, icon: t.icon, tint: t.tint, row: { ...listed(latest), ...loaded.get(key) } };
  }).sort((x, y) => y.at - x.at);
}

// Within a day, two or more rows that share a fold label become one row where the newest of them stood.
function itemsOf(day: Entry[]): Item[] {
  const folds = Map.groupBy(day.filter((e) => e.row.fold), (e) => e.row.fold!.label);
  return day.flatMap((e): Item[] => {
    const group = e.row.fold && folds.get(e.row.fold.label)!;
    if (!group || group.length < 2) return [{ entry: e }];
    return group[0] === e ? [{ fold: e.row.fold!.label, entries: group }] : [];
  });
}

function Tile({ e }: { e: Entry }) {
  return (
    <span aria-hidden="true" className={`grid h-8 w-8 flex-none place-items-center rounded-[10px] ${e.tint}`}>
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d={e.icon} /></svg>
    </span>
  );
}

// The spaces between parts collapse in the flex row but keep the link's accessible name in words.
function Body({ title, line }: { title: string; line: ReactNode }) {
  return (
    <span className="flex min-w-0 flex-1 flex-col gap-0.5">
      <span className="truncate text-[15px] font-semibold">{title}</span>
      {line != null && line !== '' && <>{' '}<span className="truncate text-[13px] text-muted">{line}</span></>}
    </span>
  );
}

const ROW = 'flex min-h-15 w-full items-center gap-3 py-2.5 pr-3 text-left text-ink no-underline hover:bg-soft sm:pr-4';

function Line({ e, kid }: { e: Entry; kid?: boolean }) {
  const newest = e.latest[0]!;
  return (
    <li>
      <a href={artifactHref(e.key, e.row.open ?? newest.kind)} className={`${ROW} ${kid ? 'pl-5 sm:pl-[60px]' : 'pl-3 sm:pl-4'}`}>
        {!kid && <Tile e={e} />}
        <Body title={e.title} line={e.row.line === undefined ? newest.summary : e.row.line} />
        {' '}
        <span className="flex flex-none items-center gap-2.5 text-[13px] text-ink-2 tabular-nums">
          {e.row.end}
          {' '}
          <time dateTime={newest.sealedAt} className="hidden min-w-[58px] text-right text-muted sm:block">{CLOCK.format(e.at)}</time>
        </span>
      </a>
    </li>
  );
}

// A day's reviews (say) as one row: how many, where from, and how many passed; it opens to list them.
function Fold({ label, entries }: { label: string; entries: Entry[] }) {
  const [open, setOpen] = useState(false);
  const ok = entries.filter((e) => e.row.fold!.ok).length;
  return (
    <li>
      <button type="button" aria-expanded={open} onClick={() => setOpen(!open)} className={`${ROW} cursor-pointer border-0 bg-transparent pl-3 font-[inherit] sm:pl-4`}>
        <Tile e={entries[0]!} />
        <Body title={`${entries.length} ${label}`} line={[...new Set(entries.map((e) => e.row.fold!.group))].join(', ')} />
        {' '}
        <span className="flex flex-none items-center gap-2.5 text-[13px] text-ink-2 tabular-nums">
          {ok > 0 && <span className="inline-flex items-center gap-1.5"><span aria-hidden="true">{ok}</span><StatusMark mark="ok" label={`${ok} approved`} /></span>}
          <svg aria-hidden="true" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" className={`text-muted transition-transform ${open ? 'rotate-90' : ''}`}><path d="M9 6l6 6-6 6" /></svg>
        </span>
      </button>
      {open && <ul className="m-0 list-none divide-y divide-line border-t border-line bg-soft/45 p-0">{entries.map((e) => <Line key={e.key} e={e} kid />)}</ul>}
    </li>
  );
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
  const name = artifactName({ kind } as SealedArtifact);
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
  // Read again when a session starts, ends or changes state, which is when a contract's version can move.
  const shape = (rows ?? []).map((r) => `${r.key}:${r.state}:${r.pendingAsks}`).join(',');
  const { data: loaded } = useLoad(() => loadRows(rows ?? []), [shape], NO_ROWS);
  // The kind menu remembers what the unfiltered list held, so choosing a kind keeps the others on offer.
  const [kinds, setKinds] = useState<string[]>([]);
  useEffect(() => void (items && !q && setKinds([...new Set(items.map((a) => a.kind))].sort())), [items]);
  const days = Map.groupBy(items ? entriesOf(items, rows, loaded) : [], (e) => dayLabel(e.at, now));
  return (
    <div className="mx-auto w-full max-w-[960px] px-4 pt-4 pb-10 sm:px-6 lg:pt-8">
      <h1 className="m-0 mb-4 hidden text-[22px] font-bold tracking-[-0.02em] lg:block">Artifacts</h1>
      <div className="mb-3">
        <SearchFilter label="Search artifacts" text={text} onText={setText} fields={fieldsOf(kinds)} values={values} onValues={setValues} />
      </div>
      {listError && <p role="alert" className="text-[13px] text-danger">{listError}</p>}
      {items?.length === 0 && <p className="text-sm text-muted">{q ? 'No artifacts match.' : 'Nothing here yet.'}</p>}
      {[...days].map(([day, entries]) => (
        <section key={day} aria-label={day} className="mb-5">
          <h2 className="m-0 mb-1.5 px-1 font-sans text-xs font-semibold tracking-wider text-muted uppercase">{day}</h2>
          <ul className="m-0 flex list-none flex-col divide-y divide-line overflow-hidden rounded-[22px] border border-line bg-surface p-0 shadow-card">
            {itemsOf(entries).map((it) => ('fold' in it ? <Fold key={`${it.fold}:${it.entries[0]!.key}`} label={it.fold} entries={it.entries} /> : <Line key={it.entry.key} e={it.entry} />))}
          </ul>
        </section>
      ))}
    </div>
  );
}
