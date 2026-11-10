import { useState } from 'react';
import { look, type Look, type Tone } from './kinds.ts';
import { SearchFilter, type FilterField, type FilterValues } from './SearchFilter.tsx';
import { age, group, sessionQuery, startOf, type Row } from './sessions.ts';
import { SpendChip } from './SpendChip.tsx';
import { ListNotices, SessionChips } from './slots.tsx';
import { useLoad } from './useLoad.ts';
import { loadRows } from './Workspace.tsx';

const TONE: Record<Tone, string> = { muted: 'text-muted', warn: 'text-warning', bad: 'text-danger' };
type Kind = 'needs' | 'running' | 'recent' | 'failed';
const DEFAULT_Q = sessionQuery('', {});

function Tile({ l, kind }: { l: Look; kind: Kind }) {
  const tint = kind === 'failed' ? 'bg-danger-bg text-danger' : kind === 'needs' ? 'bg-warning-bg text-warning' : kind === 'running' ? l.tint : 'bg-soft text-muted';
  return (
    <span aria-hidden="true" className={`relative grid h-[34px] w-[34px] flex-none place-items-center rounded-[10px] lg:h-7 lg:w-7 lg:rounded-lg ${tint}`}>
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d={l.icon} /></svg>
      {kind === 'running' && <span className="pulse-dot absolute -right-0.5 -bottom-0.5 h-2.5 w-2.5 rounded-full border-2 border-canvas bg-accent-ink" />}
    </span>
  );
}

function RowLink({ r, kind, current, now }: { r: Row; kind: Kind; current: boolean; now: number }) {
  const l = look(r);
  return (
    // The chips may be links, which cannot nest in the row's link: a subgrid lays them beside it, both placed explicitly so they may overlap,
    // and its before: box stretches the row's target under them.
    <div className={`row relative mx-2 grid min-h-11 grid-cols-[auto_minmax(0,1fr)_auto] gap-x-3 gap-y-0.5 rounded-xl px-2 py-2.5 hover:bg-soft/70 lg:gap-x-2.5 lg:rounded-[10px] lg:py-2 ${kind === 'failed' ? 'bg-danger-bg' : kind === 'needs' ? 'bg-warm' : ''}`}>
      <a
        href={`#/s/${encodeURIComponent(r.key)}`}
        aria-current={current ? 'true' : undefined}
        title={`${l.full}\n${r.key}`}
        aria-description={`${l.full} (${r.key})`}
        className="col-span-3 col-start-1 row-span-2 row-start-1 grid grid-cols-subgrid grid-rows-subgrid text-ink no-underline before:absolute before:inset-0"
      >
        <span className="row-span-2 self-start"><Tile l={l} kind={kind} /></span>
        <span className="col-span-2 flex gap-2">
          <span className={`min-w-0 flex-1 truncate text-[15px] lg:text-[13.5px] ${kind === 'recent' ? 'font-medium' : 'font-semibold'}`}>
            {l.repo && <><span className="text-[11px] font-normal text-muted">{l.repo}</span>{' '}</>}
            {l.num && <><span className="sr-only">{l.num}</span>{' '}</>}
            {l.title}
          </span>
          <time dateTime={r.createdAt} className="whitespace-nowrap text-xs tabular-nums text-muted lg:hidden">{age(r.createdAt, now)}</time>
        </span>
        <span className={`min-w-0 truncate text-[13px] lg:text-xs ${TONE[l.tone]}`}>{l.line}</span>
      </a>
      <span className="col-start-3 row-start-2 flex items-center gap-2">
        <SessionChips sessionKey={r.key} />
        <SpendChip spend={r.spend} className="text-xs text-muted" />
      </span>
    </div>
  );
}

function Section({ name, rows, kind, selected, now }: { name: string; rows: Row[]; kind: Kind; selected?: string; now: number }) {
  if (!rows.length) return null;
  return (
    <section aria-label={name}>
      <p className="m-0 px-4 pt-[18px] pb-1.5 text-xs font-semibold tracking-[.06em] text-muted uppercase lg:px-4 lg:pt-3 lg:pb-1 lg:text-[11px]">{`${name} · ${rows.length}`}</p>
      <div className="flex flex-col gap-px">
        {rows.map((r) => <RowLink key={r.key} r={r} kind={kind === 'needs' && r.failures.length ? 'failed' : kind} current={r.key === selected} now={now} />)}
      </div>
    </section>
  );
}

// The filters' choices come from every session, so picking one never hides the others from the menu.
function fieldsOf(rows: Row[]): FilterField[] {
  const option = (value: string, label = value) => ({ value, label });
  return [
    { id: 'agent', label: 'Agent', any: 'All agents', options: [...new Set(rows.map((r) => r.agent))].sort().map((a) => option(a)) },
    { id: 'start', label: 'Started by', any: 'Any start', options: [...new Set(rows.map((r) => r.key.split(':')[0]!))].sort().map((p) => option(p, startOf(`${p}:`))) },
    { id: 'view', label: 'Show', any: 'Active', options: [option('archived', 'Archived')] },
  ];
}

export function SessionList({ rows, selected, error, now }: { rows: Row[] | undefined; selected?: string; error: string; now: number }) {
  const [text, setText] = useState('');
  const [values, setValues] = useState<FilterValues>({});
  const q = sessionQuery(text, values);
  // The default view is the workspace's own rows; a search or filter asks the server again on each workspace refresh.
  const plain = q === DEFAULT_Q;
  const { data: found, error: listError } = useLoad(() => (plain ? Promise.resolve(undefined) : loadRows(q)), [q, rows], undefined);
  const shown = plain ? rows?.filter((r) => r.state !== 'archived') : found;
  const g = group(shown ?? []);
  return (
    <section aria-label="Sessions" className="flex min-h-0 flex-1 flex-col">
      <div className="px-4 pt-3 pb-2 lg:border-b lg:border-line lg:py-3">
        <SearchFilter label="Search sessions" text={text} onText={setText} fields={fieldsOf(rows ?? [])} values={values} onValues={setValues} />
      </div>
      <div className="flex-1 overflow-y-auto pb-4">
        {(error || listError) && <p role="alert" className="px-4 pt-3 text-[13px] text-danger">{error || listError}</p>}
        <ListNotices />
        {rows?.length === 0 && <p className="px-4 pt-3 text-[13px] text-muted">No sessions yet.</p>}
        {rows && rows.length > 0 && shown?.length === 0 && <p className="px-4 pt-3 text-[13px] text-muted">No sessions match.</p>}
        <Section name="Needs you" rows={g.needs} kind="needs" selected={selected} now={now} />
        <Section name="Running" rows={g.running} kind="running" selected={selected} now={now} />
        <Section name="Recent" rows={g.recent} kind="recent" selected={selected} now={now} />
      </div>
    </section>
  );
}
