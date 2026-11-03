import { useCallback, useEffect, useState } from 'react';
import type { Row } from '../../sessions.ts';
import { deleteSchedule, listSchedules, type ScheduleItem } from './api.ts';

const when = (iso: string | null) => (iso ? new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' }).format(new Date(iso)) : null);
const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];

// Plain words for the common cron shapes (daily, weekly); anything else shows the expression itself.
function cadence(s: ScheduleItem): string {
  const zone = s.tz ?? 'UTC';
  if (s.times) return `${s.times.length === 1 ? 'Once' : `${s.times.length} set times`} (${zone})`;
  const [m = '', h = '', dom, mon, dow = ''] = (s.cron ?? '').split(/\s+/);
  if (/^\d+$/.test(m) && /^\d+$/.test(h) && dom === '*' && mon === '*') {
    const clock = `${h}:${m.padStart(2, '0')} ${zone}`;
    if (dow === '*') return `Every day at ${clock}`;
    if (/^\d$/.test(dow)) return `Every ${DAYS[+dow % 7]} at ${clock}`;
  }
  return `${s.cron} (${zone})`;
}

// A key with {time} opens a new session per run, so there is no single one to link.
const sessionOf = (s: ScheduleItem) => (s.sessionKey.includes('{time}') ? undefined : s.sessionKey);

function Card({ s, remove }: { s: ScheduleItem; remove: () => void }) {
  const key = sessionOf(s);
  const next = when(s.nextRun);
  return (
    <li className="flex flex-col gap-1.5 rounded-[18px] border border-line bg-surface p-3.5 shadow-card">
      <span className="text-[14.5px] font-semibold break-words">{s.id}</span>
      <span className="text-[13px] text-ink-2">{cadence(s)}</span>
      <span className="text-[13px] text-muted">Runs {s.agent}{key ? <> in <a href={`#/s/${encodeURIComponent(key)}`} className="text-link [overflow-wrap:anywhere]">{key}</a></> : ' in a new session each time'}</span>
      <span className="text-[13px] text-muted">{next ? `Next run ${next}` : 'No upcoming run'} · {s.lastRun ? `last ran ${when(s.lastRun)}` : 'has not run yet'}</span>
      {s.missedRuns.length > 0 && <span className="w-fit rounded-full bg-warning-bg px-2 py-px text-xs text-warning">{s.missedRuns.length} missed</span>}
      {s.source === 'api' && <button type="button" onClick={remove} aria-label={`Delete ${s.id}`} className="min-h-11 w-fit cursor-pointer rounded-full border border-line bg-surface px-3 text-[13px] text-danger">Delete</button>}
    </li>
  );
}

export function Schedules(_: { rows: Row[] | undefined }) {
  const [items, setItems] = useState<ScheduleItem[]>();
  const [error, setError] = useState('');
  const load = useCallback(() => listSchedules().then((l) => (setItems(l), setError('')), (e: Error) => setError(e.message === 'no such app' ? 'Schedules is not connected.' : `Schedules is unavailable: ${e.message}`)), []);
  useEffect(() => void load(), [load]);
  const remove = (id: string) => deleteSchedule(id).then(load, (e: Error) => setError(e.message));
  return (
    <div className="mx-auto max-w-3xl px-4 py-4 lg:py-8">
      <h1 className="m-0 mb-3 hidden text-[19px] lg:block">Schedules</h1>
      {error && <p role="alert" className="text-sm text-danger">{error}</p>}
      {!items && !error && <p className="text-sm text-muted">Loading schedules…</p>}
      {items?.length === 0 && <p className="text-sm text-muted">No schedules yet.</p>}
      <ul className="m-0 flex list-none flex-col gap-2.5 p-0">{items?.map((s) => <Card key={s.id} s={s} remove={() => void remove(s.id)} />)}</ul>
    </div>
  );
}
