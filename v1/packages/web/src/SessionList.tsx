import { useState } from 'react';
import { look, type Look, type Tone } from './kinds.ts';
import { age, group, matches, startOf, type Filters, type Row } from './sessions.ts';

const field = 'rounded-full border border-line bg-soft text-[12.5px] outline-none';

const TONE: Record<Tone, string> = { plain: 'text-ink-2', muted: 'text-muted', warn: 'text-warning', ok: 'text-success', bad: 'text-danger' };
type Kind = 'needs' | 'running' | 'recent';

function Tile({ l, kind }: { l: Look; kind: Kind }) {
  const tint = kind === 'needs' ? 'bg-warning-bg text-warning' : kind === 'running' ? l.tint : 'bg-soft text-muted';
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
    <a
      href={`#/s/${encodeURIComponent(r.key)}`}
      aria-current={current ? 'true' : undefined}
      title={r.key}
      aria-description={r.key}
      className={`row mx-2 flex min-h-11 items-start gap-3 rounded-xl px-2 py-2.5 text-ink no-underline hover:bg-soft/70 lg:gap-2.5 lg:rounded-[10px] lg:py-2 ${kind === 'needs' ? 'bg-warm' : ''}`}
    >
      <Tile l={l} kind={kind} />
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="flex gap-2">
          <span className={`min-w-0 flex-1 truncate text-[15px] lg:text-[13.5px] ${kind === 'recent' ? 'font-medium' : 'font-semibold'}`}>
            {l.num && <><span className="font-medium text-muted">{l.num}</span>{' '}</>}
            {l.title}
          </span>
          <time dateTime={r.lastActivity} className="whitespace-nowrap text-xs tabular-nums text-muted lg:hidden">{age(r.lastActivity, now)}</time>
        </span>
        <span className={`truncate text-[13px] lg:text-xs ${TONE[l.tone]}`}>{l.line}</span>
      </span>
    </a>
  );
}

function Section({ name, rows, kind, selected, now }: { name: string; rows: Row[]; kind: Kind; selected?: string; now: number }) {
  if (!rows.length) return null;
  return (
    <section aria-label={name}>
      <p className="m-0 px-4 pt-[18px] pb-1.5 text-xs font-semibold tracking-[.06em] text-muted uppercase lg:px-4 lg:pt-3 lg:pb-1 lg:text-[11px]">{`${name} · ${rows.length}`}</p>
      <div className="flex flex-col gap-px">
        {rows.map((r) => <RowLink key={r.key} r={r} kind={kind} current={r.key === selected} now={now} />)}
      </div>
    </section>
  );
}

function FilterSelects({ rows, filters, set }: { rows: Row[]; filters: Filters; set: (f: Filters) => void }) {
  const agents = [...new Set(rows.map((r) => r.agent))].sort();
  const starts = [...new Set(rows.map((r) => startOf(r.key)))].sort();
  return (
    <div className="flex gap-2">
      <label className="sr-only" htmlFor="s-agent">Agent</label>
      <select id="s-agent" className={`${field} h-[30px] min-w-0 flex-1 cursor-pointer px-2`} value={filters.agent} onChange={(e) => set({ ...filters, agent: e.target.value })}>
        <option value="">All agents</option>
        {agents.map((a) => <option key={a}>{a}</option>)}
      </select>
      <label className="sr-only" htmlFor="s-trigger">Started by</label>
      <select id="s-trigger" className={`${field} h-[30px] min-w-0 flex-1 cursor-pointer px-2`} value={filters.start} onChange={(e) => set({ ...filters, start: e.target.value })}>
        <option value="">Any start</option>
        {starts.map((s) => <option key={s}>{s}</option>)}
      </select>
    </div>
  );
}

function FilterButton({ open, toggle }: { open: boolean; toggle: () => void }) {
  return (
    <button type="button" aria-expanded={open} aria-controls="filters" onClick={toggle} className="min-h-11 cursor-pointer rounded-full border border-line bg-transparent px-3.5 text-[13px] text-ink-2 hover:bg-soft hover:text-ink lg:min-h-0 lg:py-1.5">Filter</button>
  );
}

export function SessionList({ rows, selected, error, now }: { rows: Row[] | undefined; selected?: string; error: string; now: number }) {
  const [filters, setFilters] = useState<Filters>({ text: '', agent: '', start: '' });
  const [open, setOpen] = useState(false);
  const shown = (rows ?? []).filter((r) => matches(r, filters));
  const g = group(shown);
  return (
    <section aria-label="Sessions" className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-col gap-2 px-4 pt-3 pb-2 lg:border-b lg:border-line lg:py-3">
        <div className="flex items-center gap-2">
          <label className="flex h-10 min-w-0 flex-1 items-center gap-2 rounded-full bg-soft px-3 text-sm text-muted focus-within:bg-surface focus-within:shadow-ring lg:h-[34px] lg:text-[13px]">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true" className="flex-none"><circle cx="11" cy="11" r="7" /><path d="M20 20l-3.5-3.5" /></svg>
            <span className="sr-only">Search sessions</span>
            <input id="s-search" type="search" value={filters.text} onChange={(e) => setFilters({ ...filters, text: e.target.value })} placeholder="Search sessions" className="min-w-0 flex-1 border-0 bg-transparent text-ink outline-none" />
          </label>
          <FilterButton open={open} toggle={() => setOpen(!open)} />
        </div>
        <div id="filters" hidden={!open}>
          <FilterSelects rows={rows ?? []} filters={filters} set={setFilters} />
        </div>
      </div>
      <div className="flex-1 overflow-y-auto pb-4">
        {error && <p role="alert" className="px-4 pt-3 text-[13px] text-danger">{`Sessions: ${error}`}</p>}
        {rows?.length === 0 && <p className="px-4 pt-3 text-[13px] text-muted">No sessions yet.</p>}
        {rows && rows.length > 0 && !shown.length && <p className="px-4 pt-3 text-[13px] text-muted">No sessions match.</p>}
        <Section name="Needs you" rows={g.needs} kind="needs" selected={selected} now={now} />
        <Section name="Running" rows={g.running} kind="running" selected={selected} now={now} />
        <Section name="Recent" rows={g.recent} kind="recent" selected={selected} now={now} />
      </div>
    </section>
  );
}
