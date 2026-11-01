import { useState } from 'react';
import { age, group, matches, startOf, statusLine, titleOf, type Filters, type Row } from './sessions.ts';

const field = 'rounded-full border border-line bg-soft text-[12.5px] outline-none';

function Bell() {
  return (
    <svg className="mt-0.5 flex-none" width="18" height="16" viewBox="0 0 18 16" aria-hidden="true">
      <circle cx="6" cy="9" r="5" fill="#1f7a63" />
      <path d="M10 8 L15 9.5 L10 11Z" fill="#e3a92b" />
      <circle cx="6.5" cy="7.5" r="1" fill="#fff" />
      <rect x="13.5" y="0" width="2.4" height="5" rx="1.2" fill="#c27d00" />
      <circle cx="14.7" cy="6.6" r="1.2" fill="#c27d00" />
    </svg>
  );
}

// Decorative: a duckling paddling along a wave under a running session.
function Swim() {
  return (
    <span aria-hidden="true" className="relative mt-1.5 block h-3 overflow-hidden">
      <span className="waves absolute inset-x-0 bottom-0 !h-[8px] rounded-full" />
      <span className="row-swim absolute bottom-0">
        <svg width="16" height="12" viewBox="0 0 18 14" style={{ transform: 'scaleX(-1)' }}>
          <ellipse cx="10" cy="9" rx="7" ry="4.5" fill="#fdfcf7" stroke="#2f3a36" strokeOpacity=".3" />
          <circle cx="5.5" cy="5" r="3.5" fill="#1f7a63" />
          <path d="M2.4 4.3 L0 5.2 L2.4 6.2 Z" fill="#e3a92b" />
        </svg>
      </span>
    </span>
  );
}

function RowLink({ r, kind, current, now }: { r: Row; kind: 'needs' | 'running' | 'recent'; current: boolean; now: number }) {
  const lead = kind === 'needs' ? <Bell /> : <span aria-hidden="true" className={`mt-1.5 h-2 w-2 flex-none rounded-full ${kind === 'running' ? 'live-dot bg-info' : 'bg-line-strong'}`} />;
  return (
    <a
      href={`#/s/${encodeURIComponent(r.key)}`}
      aria-current={current ? 'true' : undefined}
      title={r.key}
      aria-description={r.key}
      className={`row flex min-h-11 items-start gap-2.5 px-4 py-2.5 text-ink no-underline hover:bg-soft/70 ${kind === 'needs' ? 'mx-2 rounded-xl bg-warm/60 px-3' : ''}`}
    >
      {lead}
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="truncate text-[13.5px] font-medium">{titleOf(r)}</span>
        <span className="truncate text-[12px] text-ink-2">{statusLine(r)}</span>
        {kind === 'running' && <Swim />}
      </span>
      <time dateTime={r.lastActivity} className="whitespace-nowrap text-[11.5px] tabular-nums text-muted">{age(r.lastActivity, now)}</time>
    </a>
  );
}

function Section({ name, rows, kind, selected, now }: { name: string; rows: Row[]; kind: 'needs' | 'running' | 'recent'; selected?: string; now: number }) {
  if (!rows.length) return null;
  return (
    <section aria-label={name}>
      <p className="m-0 flex items-baseline gap-1.5 px-4 pt-4 pb-1.5 text-[13px] font-semibold text-ink">
        {name} <span className="font-medium text-muted">{rows.length}</span>
      </p>
      <div className="flex flex-col gap-1">
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
    <button type="button" aria-expanded={open} aria-controls="filters" onClick={toggle} className="ml-auto min-h-11 min-w-11 cursor-pointer rounded-lg px-2 py-1 text-xs font-medium text-muted hover:bg-soft hover:text-ink lg:min-h-0 lg:min-w-0">Filter</button>
  );
}

export function SessionList({ rows, selected, error, now, compact }: { rows: Row[] | undefined; selected?: string; error: string; now: number; compact: boolean }) {
  const [filters, setFilters] = useState<Filters>({ text: '', agent: '', start: '' });
  const [open, setOpen] = useState(false);
  const shown = (rows ?? []).filter((r) => matches(r, filters));
  const g = group(shown);
  return (
    <section aria-label="Sessions" className="flex min-h-0 flex-1 flex-col">
      <div className={`flex flex-col gap-2.5 px-4 ${compact ? 'pt-3 pb-2' : 'border-b border-line py-3'}`}>
        {!compact && (
          <div className="flex items-center gap-2">
            <h1 className="m-0 text-[17px] font-semibold">Sessions</h1>
            <FilterButton open={open} toggle={() => setOpen(!open)} />
            <a href="#/new" className="inline-flex h-11 items-center rounded-full bg-ink px-3 text-[12.5px] font-semibold text-on-ink no-underline hover:bg-ink-2">New session</a>
          </div>
        )}
        <div className="flex items-center gap-1">
          <label htmlFor="s-search" className="sr-only">Search sessions</label>
          <input id="s-search" type="search" value={filters.text} onChange={(e) => setFilters({ ...filters, text: e.target.value })} placeholder="Search titles, repos, agents" className={`${field} h-[34px] w-full min-w-0 flex-1 px-3 text-[13px] transition focus:border-line-strong focus:bg-surface focus:shadow-ring`} />
          {compact && <FilterButton open={open} toggle={() => setOpen(!open)} />}
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
