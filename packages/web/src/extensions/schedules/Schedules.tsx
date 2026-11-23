import { useEffect, useRef, useState } from 'react';
import { sessionTitle } from '../../kinds.ts';
import { MoreIcon } from '../../MoreIcon.tsx';
import { titleOf, type Row } from '../../sessions.ts';
import { useLoad } from '../../useLoad.ts';
import { useMenu } from '../../useMenu.ts';
import { deleteSchedule, listSchedules, type ScheduleItem } from './api.ts';

const fmt = (o: Intl.DateTimeFormatOptions) => new Intl.DateTimeFormat('en-US', o);
const WHEN = fmt({ month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
const CLOCK = fmt({ hour: 'numeric', minute: '2-digit' });
const DAY = fmt({ weekday: 'short', month: 'short', day: 'numeric' });
const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
const UNDO_MS = 5000;
const REPEAT = 'M17 2l4 4-4 4M3 11V9a3 3 0 0 1 3-3h15M7 22l-4-4 4-4M21 13v2a3 3 0 0 1-3 3H3';
const ICON_BTN = 'relative grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-muted hover:bg-soft hover:text-ink focus-visible:outline-2 focus-visible:outline-link';
const ITEM = 'flex min-h-11 w-full cursor-pointer items-center border-0 bg-transparent px-3 text-left text-[14px] no-underline hover:bg-soft focus-visible:bg-soft focus-visible:outline-none';

// Plain words for the common cron shapes (daily, weekly); anything else shows the expression itself.
function cadence(cron: string, zone = 'UTC'): string {
  const [m = '', h = '', dom, mon, dow = ''] = cron.split(/\s+/);
  if (/^\d+$/.test(m) && /^\d+$/.test(h) && dom === '*' && mon === '*') {
    const clock = `${h}:${m.padStart(2, '0')} ${zone}`;
    if (dow === '*') return `Every day at ${clock}`;
    if (/^\d$/.test(dow)) return `Every ${DAYS[+dow % 7]} at ${clock}`;
  }
  return `${cron} (${zone})`;
}

// A key with {time} opens a new session per run, so there is no single one to link or name.
const sessionOf = (s: ScheduleItem) => (s.sessionKey.includes('{time}') ? undefined : s.sessionKey);

function titleFor(s: ScheduleItem, rows: Row[] | undefined): string {
  const key = sessionOf(s);
  if (!key) return s.agent;
  return titleOf(rows?.find((r) => r.key === key) ?? { key, agent: s.agent, title: undefined });
}

type Entry = { s: ScheduleItem; title: string; time: string; repeat: boolean };
type Group = { day: string; note?: string; entries: Entry[] };

function dayNote(d: Date, now: Date): string | undefined {
  const tomorrow = new Date(now);
  tomorrow.setDate(now.getDate() + 1);
  return d.toDateString() === now.toDateString() ? 'Today' : d.toDateString() === tomorrow.toDateString() ? 'Tomorrow' : undefined;
}

/** Schedules by local day of their next run, soonest first; a run in the same minute as the row above repeats no time. */
function agenda(items: ScheduleItem[], rows: Row[] | undefined, now: Date): Group[] {
  const at = (s: ScheduleItem) => (s.nextRun ? Date.parse(s.nextRun) : Infinity);
  const sorted = items.toSorted((a, b) => (at(a) === at(b) ? 0 : at(a) < at(b) ? -1 : 1));
  const groups: Group[] = [];
  for (const s of sorted) {
    const next = s.nextRun ? new Date(s.nextRun) : undefined;
    const day = next ? DAY.format(next) : 'No upcoming run';
    const time = next ? CLOCK.format(next) : '';
    let g = groups.at(-1);
    if (g?.day !== day) groups.push((g = { day, note: next && dayNote(next, now), entries: [] }));
    const prev = g.entries.at(-1);
    g.entries.push({ s, title: titleFor(s, rows), time, repeat: !!time && prev?.time === time });
  }
  return groups;
}

function RowMenu({ title, href, remove }: { title: string; href?: string; remove: () => void }) {
  const menu = useMenu();
  return (
    <div ref={menu.root} onKeyDown={menu.keys} className="relative flex-none">
      <button ref={menu.button} type="button" aria-label={`More for ${title}`} aria-haspopup="menu" aria-expanded={menu.open} onClick={menu.toggle} className={ICON_BTN}><MoreIcon /></button>
      {menu.open && (
        <div role="menu" aria-label={`More for ${title}`} className="absolute right-0 z-20 mt-1 min-w-44 rounded-xl border border-line-strong bg-surface py-1 shadow-float">
          {href && <a role="menuitem" href={href} onClick={() => menu.close(false)} className={`${ITEM} text-ink`}>Open session</a>}
          <button type="button" role="menuitem" onClick={() => (menu.close(false), remove())} className={`${ITEM} text-danger`}>Delete schedule</button>
        </div>
      )}
    </div>
  );
}

function Detail({ s }: { s: ScheduleItem }) {
  const who = sessionOf(s) ? s.agent : 'New session each run';
  // One set time is the default and says nothing; several say how many are coming.
  if (!s.cron) return <span className="text-[12.5px] text-muted">{(s.times?.length ?? 0) > 1 ? `${who} · ${s.times!.length} set times` : who}</span>;
  return (
    <span className="truncate text-[12.5px] text-muted">
      {who} · <span className="text-accent-ink">
        <svg width="12" height="12" className="mr-1 inline align-[-1px]" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={REPEAT} /></svg>
        {cadence(s.cron, s.tz)}
      </span>
      {s.lastRun && <span> · last ran {WHEN.format(new Date(s.lastRun))}</span>}
    </span>
  );
}

function Item({ e, remove }: { e: Entry; remove: () => void }) {
  const { s, title } = e;
  const key = sessionOf(s);
  const href = key && `#/s/${encodeURIComponent(key)}`;
  const icon = sessionTitle({ key: key ?? s.sessionKey, agent: s.agent, title: undefined }).icon;
  return (
    <li className="relative flex min-h-14 items-center gap-3 py-2 pr-1.5 pl-3.5 first:rounded-t-[18px] last:rounded-b-[18px] hover:bg-soft">
      <span className="w-[4.5rem] flex-none text-[13.5px] font-semibold tabular-nums"><span className={e.repeat ? 'sr-only' : ''}>{e.time}</span></span>
      <span aria-hidden="true" className="hidden size-8 flex-none place-items-center rounded-lg bg-soft text-ink-2 sm:grid">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d={icon} /></svg>
      </span>
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        {/* The link's box covers the row, so a tap anywhere opens the session; the menu sits above it. */}
        {href ? <a href={href} className="truncate text-[14.5px] font-semibold text-ink no-underline after:absolute after:inset-0">{title}</a> : <span className="truncate text-[14.5px] font-semibold">{title}</span>}
        <Detail s={s} />
      </span>
      {s.missedRuns.length > 0 && <span className="flex-none rounded-full bg-warning-bg px-2 py-px text-xs text-warning">{s.missedRuns.length} missed</span>}
      <RowMenu title={title} href={href} remove={remove} />
    </li>
  );
}

// A DELETE that failed after the page went away; the next visit says so, since the row is back.
let lost: string | undefined;

// Deleting hides the row at once; the DELETE goes out once, when the Undo window lapses, another delete starts, the page unmounts or the tab closes.
function useUndoDelete(items: ScheduleItem[] | undefined, reload: () => void, setError: (e: string) => void) {
  const [pending, setPending] = useState<Entry>();
  const [hidden, setHidden] = useState<ReadonlySet<string>>(new Set());
  const held = useRef<{ e: Entry; timer: ReturnType<typeof setTimeout> }>(undefined);
  const alive = useRef(true);
  const show = (id: string, on: boolean) => setHidden((h) => new Set([...h].filter((x) => x !== id).concat(on ? [] : [id])));
  const failed = (e: Entry, err: Error) => (alive.current ? (show(e.s.id, true), setError(err.message)) : (lost = `Could not delete ${e.title}: ${err.message}`));
  const settle = (send: boolean) => {
    const p = held.current;
    if (!p) return;
    clearTimeout(p.timer);
    held.current = undefined;
    setPending(undefined);
    if (!send) return show(p.e.s.id, true);
    deleteSchedule(p.e.s.id).then(() => (setError(''), reload()), (err: Error) => failed(p.e, err));
  };
  // A tab close or reload never unmounts React, so pagehide settles the window too.
  useEffect(() => {
    alive.current = true;
    if (lost) setError(lost);
    lost = undefined;
    const flush = () => settle(true);
    addEventListener('pagehide', flush);
    return () => {
      removeEventListener('pagehide', flush);
      alive.current = false;
      settle(true);
    };
  }, []);
  // A row the server no longer lists needs hiding no more.
  useEffect(() => void (items && setHidden((h) => new Set([...h].filter((id) => items.some((s) => s.id === id))))), [items]);
  const remove = (e: Entry) => {
    settle(true);
    show(e.s.id, false);
    setPending(e);
    held.current = { e, timer: setTimeout(() => settle(true), UNDO_MS) };
  };
  return { pending, hidden, remove, undo: () => settle(false) };
}

// The row that held the menu is gone, so focus moves to Undo, which would otherwise be the last stop in the page.
function Toast({ title, undo }: { title: string; undo: () => void }) {
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => button.current?.focus(), []);
  return (
    <div role="status" className="fixed bottom-[calc(64px+var(--safe-b))] left-1/2 z-40 flex max-w-[calc(100vw-2rem)] -translate-x-1/2 items-center gap-3 rounded-xl border border-line-strong bg-surface py-1 pr-1 pl-4 text-[13.5px] text-ink shadow-lift lg:bottom-8 lg:left-[calc(50%+9rem)]">
      <span className="truncate">Deleted {title}</span>
      <button ref={button} type="button" onClick={undo} className="min-h-11 flex-none cursor-pointer rounded-lg border-0 bg-transparent px-3 font-semibold text-accent-ink hover:bg-soft focus-visible:outline-2 focus-visible:outline-link">Undo</button>
    </div>
  );
}

export function Schedules({ rows }: { rows: Row[] | undefined }) {
  const { data: items, error: loadError, reload } = useLoad<ScheduleItem[] | undefined>(listSchedules, [], undefined);
  const [removeError, setRemoveError] = useState('');
  const { pending, hidden, remove, undo } = useUndoDelete(items, reload, setRemoveError);
  const error = removeError || (loadError && (loadError === 'no such app' ? 'Schedules is not connected.' : `Schedules is unavailable: ${loadError}`));
  const shown = items?.filter((s) => !hidden.has(s.id));
  return (
    <div className="mx-auto max-w-3xl px-4 py-4 lg:py-8">
      <h1 className="m-0 mb-3 hidden text-[19px] lg:block">Schedules</h1>
      {error && <p role="alert" className="text-sm text-danger">{error}</p>}
      {!items && !error && <p className="text-sm text-muted">Loading schedules…</p>}
      {shown?.length === 0 && <p className="text-sm text-muted">No schedules yet.</p>}
      {agenda(shown ?? [], rows, new Date()).map((g) => (
        <section key={g.day} aria-label={g.note ? `${g.day}, ${g.note}` : g.day} className="mb-4">
          <h2 className="m-0 mb-1.5 flex gap-2 px-1 text-[11.5px] font-semibold tracking-wide text-muted uppercase">{g.day}{g.note && <span className="font-medium normal-case">{g.note}</span>}</h2>
          <ul className="m-0 list-none divide-y divide-line rounded-[18px] border border-line bg-surface p-0 shadow-card">
            {g.entries.map((e) => <Item key={e.s.id} e={e} remove={() => remove(e)} />)}
          </ul>
        </section>
      ))}
      {pending && <Toast key={pending.s.id} title={pending.title} undo={undo} />}
    </div>
  );
}
