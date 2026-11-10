import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import type { SealedArtifact } from '../../api.ts';
import { sentence } from '../../extension.ts';
import type { Row } from '../../sessions.ts';
import { CURRENT, Full, JOBS, ORDER, RUNNABLE, Short, brief, titleOf, whatIs } from './cards.tsx';
import { runJob, sleeperLeague, upcoming, type Live, type Run } from './api.ts';
import { NewsList, TitleTick, Verdict, slot, weekday, when, type News } from './parts.tsx';
import { sessionsOf, useResults, wanted, weekResult, type Result, type Sess } from './week.ts';
import { Dialog } from '../../Dialog.tsx';
import { MoreIcon } from '../../MoreIcon.tsx';
import { Duck } from '../../Pond.tsx';

// The slice of @quack/sleeper's lineup card the page reads; the card file is the contract.
type Player = { id: string; name: string; pos: string; team: string | null; inj: string | null };
type Game = { opponent: string; home: boolean; kickoff: string | null; locked: boolean; status: string | null };
type Starter = { slot: string; player: Player; proj: number; action: 'start' | 'keep'; locked: boolean; replaces: Player | null; game: Game | null; why: string; news?: News };
export type Card = { kind: 'lineup'; league: string; week: number; team: string; opponent: string | null; my_proj: number; opp_proj: number | null; summary: string; starters: Starter[]; bench: Array<{ player: Player; proj: number }> };

const NO_CHANGES = 'No changes: your lineup stands';
const unchanged = (card: Card) => card.starters.every((s) => s.action === 'keep');

// Locks at the earliest kickoff among slots still open; undefined when every game has started.
const lockTime = (card: Card | undefined) => {
  const kicks = (card?.starters ?? []).filter((s) => !s.locked && s.game?.kickoff).map((s) => Date.parse(s.game!.kickoff!));
  return kicks.length ? Math.min(...kicks) : undefined;
};

function StarterRow({ s }: { s: Starter }) {
  const change = s.action === 'start';
  const g = s.game;
  return (
    <li className={`flex flex-col gap-1 px-4 py-3 ${change ? 'bg-success-bg/60' : ''}`}>
      <div className="flex min-w-0 items-baseline gap-2">
        <span className={`w-10 flex-none font-mono text-[11.5px] ${change ? 'text-success' : 'text-muted'}`}>{s.slot}</span>
        <span className="min-w-0 flex-1 break-words text-[14px] font-medium">
          {s.player.name}
          {change && <span className="ml-1.5 rounded-md bg-success-bg px-1.5 py-0.5 text-[10px] font-bold text-success">START</span>}
          {s.player.inj && <span className="ml-1.5 rounded-md bg-warning-bg px-1.5 py-0.5 text-[10px] font-bold text-warning">{s.player.inj}</span>}
          {s.locked && <span className="ml-1.5 rounded-md bg-soft px-1.5 py-0.5 text-[10px] font-bold text-muted">LOCKED</span>}
        </span>
        <span className="flex-none text-[14px] font-semibold tabular-nums">{s.proj.toFixed(1)}</span>
      </div>
      <div className="flex flex-col gap-1 pl-12">
        <p className="m-0 text-[12px] text-muted">
          {`${s.player.pos} · ${s.player.team ?? 'FA'}`}
          {g && ` · ${g.home ? 'vs' : '@'} ${g.opponent}${g.kickoff ? ` · ${when(g.kickoff)}` : ''}`}
          {s.replaces && ` · replaces ${s.replaces.name}`}
        </p>
        <p className="m-0 break-words text-[13px] text-ink-2">{s.why}</p>
        <NewsList items={s.news ?? []} />
      </div>
    </li>
  );
}

export function LineupCard({ card, artifact, sessionKey }: { card: Card; artifact: SealedArtifact; sessionKey: string }) {
  return (
    <article aria-label={`Week ${card.week} lineup`} className="overflow-hidden rounded-[22px] border border-line bg-surface shadow-card">
      <div className="flex flex-col gap-2 border-b border-line px-4 py-3">
        <div className="flex items-center gap-2">
          <p className="m-0 text-sm font-semibold">Start / sit</p>
          <TitleTick artifact={artifact} />
          <a href={`#/s/${encodeURIComponent(sessionKey)}`} className="ml-auto inline-flex min-h-11 items-center rounded-lg px-2 text-xs font-medium text-link no-underline hover:bg-soft lg:min-h-0 lg:py-1">Ask about this</a>
        </div>
        <Verdict artifact={artifact} />
      </div>
      <div className="grid grid-cols-[1fr_auto_1fr] items-center gap-3 border-b border-line px-4 py-4">
        <div className="min-w-0"><p className="m-0 truncate text-[13px] text-muted">{card.team}</p><p className="m-0 text-[24px] font-semibold tabular-nums tracking-[-0.02em]">{card.my_proj.toFixed(1)}</p></div>
        <span className="text-[12px] text-muted">vs</span>
        <div className="min-w-0 text-right"><p className="m-0 truncate text-[13px] text-muted">{card.opponent ?? 'No opponent'}</p><p className="m-0 text-[24px] font-semibold tabular-nums tracking-[-0.02em]">{card.opp_proj?.toFixed(1) ?? '–'}</p></div>
      </div>
      {unchanged(card)
        ? <p className="m-0 border-b border-line bg-soft px-4 py-2.5 text-[13px] text-ink-2">{NO_CHANGES}</p>
        : <p className="m-0 border-b border-line bg-warm px-4 py-2.5 text-[13px] text-ink-2 break-words">{card.summary}</p>}
      <ul className="m-0 flex list-none flex-col divide-y divide-line p-0">{card.starters.map((s) => <StarterRow key={s.slot} s={s} />)}</ul>
      <details className="border-t border-line px-4 py-3 text-[13px]">
        <summary className="flex min-h-11 cursor-pointer items-center text-ink-2 lg:min-h-6">{`Bench · ${card.bench.length}`}</summary>
        <ul className="m-0 mt-2 flex list-none flex-col gap-1 p-0">
          {card.bench.map((b) => <li key={b.player.id} className="flex gap-2"><span className="min-w-0 flex-1 break-words">{`${b.player.name} · ${b.player.pos}`}</span><span className="tabular-nums text-ink-2">{b.proj.toFixed(1)}</span></li>)}
        </ul>
      </details>
    </article>
  );
}


const DAY = 864e5;
// One color per job so a run keeps its color week to week; tokens that read in both themes.
const toneOf = (job: string) => JOBS[job]?.tone ?? 'bg-muted';

// The duck art faces left; mirrored so it faces forward in time.
const today = <Duck role="img" aria-label="Today" width="26" height="20" style={{ transform: 'scaleX(-1)' }} />;

// Midnight today in the league zone; a DST change inside the day shifts it by an hour at most.
function dayStart(now: number) {
  const p = Object.fromEntries(new Intl.DateTimeFormat('en-US', { timeZone: 'America/New_York', hourCycle: 'h23', hour: 'numeric', minute: 'numeric', second: 'numeric' }).formatToParts(now).map((x) => [x.type, Number(x.value)]));
  return now - ((p.hour! * 60 + p.minute!) * 60 + p.second!) * 1000 - (now % 1000);
}

// Runs at the same moment share a pin and a line ("Recap and retro"); trends run daily and stay out of the week.
function pins(runs: Run[], now: number) {
  const week = runs.filter((r) => r.job !== 'trends' && Date.parse(r.at) > now && Date.parse(r.at) < now + 7 * DAY);
  return [...new Set(week.map((r) => r.at))].map((at) => {
    const jobs = week.filter((r) => r.at === at).map((r) => r.job);
    return { at: Date.parse(at), tone: toneOf(jobs[0]!), label: sentence(jobs.map((j) => (JOBS[j]?.label ?? j).toLowerCase()).join(' and ')) };
  });
}

type Item = { at: number; tone: string; label: string; lock?: boolean };

function Track({ start, now, items }: { start: number; now: number; items: Item[] }) {
  const pct = (ms: number) => `${(((ms - start) / (7 * DAY)) * 100).toFixed(2)}%`;
  const inside = (ms: number) => ms >= start && ms < start + 7 * DAY;
  return (
    <div className="relative mx-3 h-14">
      <div aria-hidden="true" className="absolute top-8 right-0 left-0 h-0.5 rounded-full bg-muted/60" />
      <div aria-hidden="true" data-elapsed className="absolute top-8 left-0 h-0.5 rounded-full bg-line" style={{ width: pct(now) }} />
      {DAYS_AHEAD.map((i) => {
        const ms = start + i * DAY;
        const name = weekday(ms + DAY / 2);
        return (
          <div key={i} aria-current={i === 0 ? 'date' : undefined} className="absolute top-0 flex flex-col items-start" style={{ left: pct(ms) }}>
            <span className={`text-[12px] ${i === 0 ? 'font-semibold text-success' : 'text-muted'}`}><span aria-hidden="true" className="sm:hidden">{name[0]}</span><span className="max-sm:sr-only">{name}</span></span>
            <span aria-hidden="true" className="mt-1 h-2.5 w-px bg-muted" />
          </div>
        );
      })}
      {items.filter((m) => inside(m.at)).map((m) => m.lock
        ? <span key="lock" aria-hidden="true" data-marker="lock" className="absolute top-5 h-7 w-1 -translate-x-1/2 rounded-full bg-danger" style={{ left: pct(m.at) }} />
        : <span key={m.at} aria-hidden="true" data-marker="run" className={`absolute top-[26px] h-3.5 w-3.5 -translate-x-1/2 rounded-full ${m.tone}`} style={{ left: pct(m.at) }} />)}
      <div className="absolute top-[14px] -translate-x-1/2" style={{ left: pct(now) }}>{today}</div>
    </div>
  );
}

const DAYS_AHEAD = [0, 1, 2, 3, 4, 5, 6];

// A run past the end of the track falls on the same weekday as today, so it says which one.
const listTime = (ms: number, end: number) => (ms >= end ? `Next ${slot(ms)}` : slot(ms));

function WeekStrip({ lock, now, runs }: { lock?: number; now: number; runs: Run[] }) {
  const end = dayStart(now) + 7 * DAY;
  const items: Item[] = [...pins(runs, now), ...(lock === undefined ? [] : [{ at: lock, tone: 'bg-danger', label: lock > now ? 'Lineup locks' : 'Lineup locked', lock: true }])].sort((a, b) => a.at - b.at);
  return (
    <section aria-label="This week" className="rounded-[22px] border border-line bg-surface p-4 shadow-card">
      <Track start={dayStart(now)} now={now} items={items} />
      <ul className="m-0 mt-3 flex list-none flex-col gap-1.5 p-0 text-[14px]">
        {items.map((m) => (
          <li key={`${m.lock}${m.at}`} className={`flex items-center gap-2 ${m.lock ? 'font-semibold' : ''} ${m.at < now ? 'opacity-60' : ''}`}>
            <span aria-hidden="true" className={`flex-none ${m.lock ? 'h-3.5 w-1 rounded-full' : 'h-2.5 w-2.5 rounded-full'} ${m.tone}`} />
            <span className="min-w-0 flex-1 break-words">{m.label}</span>
            <span className={m.lock ? 'text-danger' : 'text-ink-2'}>{listTime(m.at, end)}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}

const btn = 'inline-flex min-h-11 cursor-pointer items-center justify-center rounded-lg border-0 bg-transparent px-2 text-xs font-medium text-link no-underline hover:bg-soft lg:min-h-0 lg:py-1';

function ResultCard({ r, compact }: { r: Result; compact?: boolean }) {
  const [open, setOpen] = useState(false);
  const title = titleOf(r);
  if (open && r.job === 'lineup' && Array.isArray(r.card.starters)) {
    return <div className="flex flex-col gap-1"><LineupCard card={r.card as Card} artifact={r.artifact} sessionKey={r.key} /><button type="button" onClick={() => setOpen(false)} className={`${btn} self-end`} aria-label={`Close ${title}`}>Close</button></div>;
  }
  return (
    <article id={`sleeper-${r.job}`} aria-label={title} className="scroll-mt-4 rounded-[22px] border border-line bg-surface shadow-card">
      <div className="flex items-center gap-3 px-4 pt-3">
        <div className="min-w-0 flex-1">
          <h2 className="m-0 flex items-center gap-2 text-[15px] font-semibold">{title}<TitleTick artifact={r.artifact} /></h2>
          <p className="m-0 text-[12px] text-muted">{compact ? whatIs(r) : brief(r).sub}</p>
        </div>
        <button type="button" aria-expanded={open} aria-label={`${open ? 'Close' : 'Open'} ${title}`} onClick={() => setOpen(!open)} className={btn}>{open ? 'Close' : 'Open ›'}</button>
      </div>
      <div className="flex flex-col gap-2 px-4 pt-1 pb-4">
        {open ? <Full r={r} /> : !compact && <Short r={r} />}
        <Verdict artifact={r.artifact} />
        {open && <a href={`#/s/${encodeURIComponent(r.key)}`} className={`${btn} self-start`}>Ask about this</a>}
      </div>
    </article>
  );
}

function Outcome({ week, results }: { week: number; results: Result[] }) {
  const o = weekResult(week, results);
  if (!o) return null;
  const label = o.won === undefined ? 'Tied' : o.won ? 'Won' : 'Lost';
  return (
    <section aria-label={`Week ${week} result`} className="rounded-[22px] border border-line bg-surface p-4 shadow-card">
      <p className={`m-0 text-[12px] font-bold uppercase tracking-[0.08em] ${o.won ? 'text-success' : 'text-ink-2'}`}>{label}</p>
      <p className="m-0 mt-1 text-[28px] font-semibold tabular-nums tracking-[-0.02em]">{o.opp === undefined ? o.started : `${o.started} to ${o.opp}`}</p>
      <p className="m-0 mt-1 text-[14px] text-ink-2">{`Left ${o.left} on the bench (best lineup ${o.best})`}</p>
    </section>
  );
}

// A past week leads with how it went.
const PAST = ['digest', 'retro', ...ORDER.filter((j) => j !== 'digest' && j !== 'retro')];

/** A job's session while it starts ('') or runs (its key); undefined when it can be run. */
type LiveKey = (job: string) => string | undefined;
type SheetProps = { found: Result[]; runs: Run[]; live: LiveKey; status: string; onRun: (job: string, name: string) => void; onClose: () => void };

const jobRow = 'flex min-h-14 w-full items-center gap-3 rounded-xl border-0 bg-transparent px-3 py-2 text-left no-underline hover:bg-soft focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-link';
const glyph = (d: string) => <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="flex-none text-muted"><path d={d} /></svg>;

// The whole row runs the job; once started it becomes a link to the run, so a second tap cannot start another.
function JobRow({ job, name, line, live, onRun }: { job: string; name: string; line: string; live: string | undefined; onRun: SheetProps['onRun'] }) {
  const text = (second: string) => (
    <span className="flex min-w-0 flex-1 flex-col">
      <span className="text-[14px] font-medium text-ink">{name}</span>{' '}
      <span id={`job-${job}`} className="truncate text-[12px] text-muted">{second}</span>
    </span>
  );
  if (live) return <a href={`#/s/${encodeURIComponent(live)}`} className={jobRow}>{text('Running · open')}{glyph('M9 6l6 6-6 6')}</a>;
  const starting = live === '';
  return (
    <button type="button" aria-label={`Run ${name}`} aria-describedby={`job-${job}`} disabled={starting} onClick={() => onRun(job, name)} className={`${jobRow} cursor-pointer disabled:cursor-default disabled:hover:bg-transparent`}>
      {text(starting ? 'Starting…' : line)}
      {!starting && glyph('M8 5l11 7-11 7z')}
    </button>
  );
}

function JobSheet({ found, runs, live, status, onRun, onClose }: SheetProps) {
  return (
    // The frame only places the sheet; clicks beside it reach the backdrop beneath.
    <Dialog label="Run a job" onClose={onClose} className="pointer-events-none fixed inset-0 z-50 flex items-end justify-center sm:items-center">
      <div className="pointer-events-auto relative flex max-h-[85dvh] w-full flex-col rounded-t-[22px] border border-line bg-canvas pt-3 shadow-lift sm:max-w-md sm:rounded-[22px]">
        <div className="flex items-center gap-2 px-4">
          <h2 className="m-0 flex-1 text-[16px] font-semibold">Run a job</h2>
          <button type="button" data-autofocus aria-label="Close" title="Close" onClick={onClose} className={nav}>
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>
          </button>
        </div>
        {status && <p role="status" className="m-0 mb-1 px-4 text-[13px] text-danger">{status}</p>}
        <ul className="m-0 flex min-h-0 list-none flex-col overflow-y-auto overscroll-contain px-1 pt-1 pb-[calc(0.75rem+var(--safe-b))]">
          {RUNNABLE.map((job) => {
            const name = JOBS[job]!.label;
            const last = found.find((r) => r.job === job);
            const next = runs.find((r) => r.job === job);
            const line = [last ? `${titleOf(last)} · ${weekday(last.artifact.sealedAt)}` : 'Nothing yet', next && `next ${slot(Date.parse(next.at))}`].filter(Boolean).join(' · ');
            return <li key={job}><JobRow job={job} name={name} line={line} live={live(job)} onRun={onRun} /></li>;
          })}
        </ul>
      </div>
    </Dialog>
  );
}

const nav = 'grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-[22px] text-ink-2 hover:bg-soft focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link disabled:cursor-default disabled:opacity-30 disabled:hover:bg-transparent';

type HeaderProps = { week?: number; team?: string; prev?: number; next?: number; past: boolean; onWeek: (w: number | undefined) => void; onMenu: () => void };

// The trailing action has a fixed column mirrored by an empty one, so Week N stays centred whichever action shows.
function Header({ week, team, prev, next, past, onWeek, onMenu }: HeaderProps) {
  return (
    <header className="grid grid-cols-[2.75rem_minmax(0,1fr)_2.75rem] items-center">
      <span aria-hidden="true" />
      <div className="flex min-w-0 items-center justify-center gap-1">
        <button type="button" aria-label="Earlier week" title="Earlier week" disabled={prev === undefined} onClick={() => onWeek(prev)} className={nav}>‹</button>
        <div className="min-w-0 text-center">
          <h1 className="m-0 text-[22px] font-bold tracking-[-0.02em]">{week === undefined ? 'Sleeper' : `Week ${week}`}</h1>
          {team && <p className="m-0 truncate text-[13px] text-ink-2">{team}</p>}
        </div>
        <button type="button" aria-label="Later week" title="Later week" disabled={next === undefined} onClick={() => onWeek(next)} className={nav}>›</button>
      </div>
      {past
        ? <button type="button" aria-label="This week" title="This week" onClick={() => onWeek(undefined)} className={nav}><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M4 6h16v14H4zM4 10h16M8 3v4M16 3v4" /><circle cx="12" cy="15" r="1.6" fill="currentColor" /></svg></button>
        : <button type="button" aria-label="Run a job" title="Run a job" aria-haspopup="dialog" onClick={onMenu} className={nav}><MoreIcon /></button>}
    </header>
  );
}

function Lineup({ r, running }: { r: Result | undefined; running: boolean }) {
  if (r && Array.isArray(r.card.starters)) return <div id="sleeper-lineup" className="scroll-mt-4"><LineupCard card={r.card as Card} artifact={r.artifact} sessionKey={r.key} /></div>;
  return (
    <section id="sleeper-lineup" aria-label="Start / sit" className="rounded-[22px] border border-line bg-surface p-4 shadow-card">
      <h2 className="m-0 text-[15px] font-semibold">Start / sit</h2>
      <p className="m-0 mt-1 text-[13px] text-ink-2">{running ? 'Running now' : 'No lineup yet this week'}</p>
    </section>
  );
}

function Alerts({ note, error }: { note: string; error: string }) {
  return (
    <>
      {note && <p role="alert" className="m-0 text-[13px] text-ink-2">{note}</p>}
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
    </>
  );
}

// A job started here stays live (pending, then its session) until the league list shows that session itself.
function useJobs(all: Sess[], current: number | undefined, started: () => void) {
  const [mine, setMine] = useState<Record<string, string>>({});
  const [status, setStatus] = useState('');
  const run = useCallback((job: string) => {
    setStatus('');
    setMine((m) => ({ ...m, [job]: '' }));
    runJob(job).then(({ sessionKey }) => (setMine((m) => ({ ...m, [job]: sessionKey })), started()), (e: Error) => {
      setMine(({ [job]: _, ...m }) => m);
      setStatus(`Could not start: ${e.message}`);
    });
  }, []);
  const running = (job: string) => all.find((s) => s.job === job && s.week === current && s.running)?.key;
  const live: LiveKey = (job) => {
    const own = mine[job];
    return running(job) ?? (own === '' || (own && !all.some((s) => s.key === own)) ? own : undefined);
  };
  return { run, running, live, status };
}

export function Sleeper({ rows, now = Date.now }: { rows: Row[] | undefined; now?: () => number }) {
  const [league, setLeague] = useState<string>();
  const [unconfigured, setUnconfigured] = useState('');
  const [picked, setPicked] = useState<number>();
  const [menu, setMenu] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const [live, setLive] = useState<Live>({ runs: [] });
  const runs = live.runs;
  useEffect(() => void upcoming().then(setLive), []);
  useEffect(() => void sleeperLeague().then(setLeague, () => setUnconfigured('Sleeper is not set up on this server.')), []);
  const all = useMemo(() => sessionsOf(league, rows), [league, rows]);
  // Sleeper's live week leads when the service reports it, so a week with no runs yet is still the current one.
  const weeks = [...new Set([...all.map((s) => s.week), ...(live.week ? [live.week] : [])])].sort((a, b) => a - b);
  const current = weeks.at(-1);
  const week = picked !== undefined && weeks.includes(picked) ? picked : current;
  const past = week !== current;
  const need = useMemo(() => (week === undefined ? [] : wanted(all, week, current!)), [all, week, current]);
  const { results, error } = useResults(need, refresh);
  const found = need.flatMap((s) => results[s.key] ?? []);
  const jobs = useJobs(all, current, () => setRefresh((n) => n + 1));
  const lineup = found.find((r) => r.job === 'lineup' && r.week === week);
  const shown = (past ? PAST : CURRENT).flatMap((j) => found.filter((r) => r.job === j && (!past || r.week === week)));
  const team = found.find((r) => r.card.team)?.card.team as string | undefined;
  return (
    <div className="mx-auto flex w-full max-w-[1240px] flex-col gap-4 px-4 pt-4 pb-10 sm:px-6">
      <Header week={week} team={team} prev={weeks.filter((w) => w < (week ?? 0)).at(-1)} next={weeks.find((w) => w > (week ?? 0))} past={past} onWeek={setPicked} onMenu={() => setMenu(true)} />
      <Alerts note={unconfigured} error={error} />
      {league && week === undefined && <p className="m-0 text-[13px] text-ink-2">No results yet. Run a job from the menu.</p>}
      {week !== undefined && !past && <><WeekStrip lock={lockTime(lineup?.card as Card | undefined)} now={now()} runs={runs} /><Lineup r={lineup} running={jobs.running('lineup') !== undefined} /></>}
      {past && <Outcome week={week!} results={found} />}
      {shown.map((r) => <ResultCard key={r.key} r={r} compact={past} />)}
      {past && shown.length === 0 && <p className="m-0 text-[13px] text-ink-2">Nothing from this week.</p>}
      {menu && <JobSheet found={found} runs={runs} live={jobs.live} status={jobs.status} onRun={jobs.run} onClose={() => setMenu(false)} />}
    </div>
  );
}
