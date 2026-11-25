import { useEffect, useRef, useState, type ReactNode } from 'react';
import { listSessions, probeSignIn, type Version } from './api.ts';
import { Home } from './Home.tsx';
import { sessionTitle } from './kinds.ts';
import { multiScope, rowOf, type Row } from './sessions.ts';
import { SessionView } from './SessionView.tsx';

// Relative times ("3m") move on without a write; this redraws them and reads nothing.
const TICK_MS = 60_000;
// A non-200 answer ends an EventSource for good, so the page reopens it after this.
const REOPEN_MS = 30_000;
const WATCH = './sessions:watch';

// The row carries its pending-ask count and its last run, so the tick is this one list.
export async function loadRows(q?: string): Promise<Row[]> {
  // The server orders by last activity (key breaks ties); a refresh moves a row that worked, as it inserts a new one.
  const sessions = await listSessions(q);
  // A repo prefix only earns its space when reviews come from more than one repo.
  const multiRepo = multiScope(sessions.map((s) => s.key));
  return sessions.map((s) => rowOf(s, multiRepo));
}

/** The session list, re-read on mount, on every (re)open of the Sessions API change stream and on each change it reports; `served` is the build its hello names. */
export function useWorkspaceData() {
  const [rows, setRows] = useState<Row[]>();
  const [error, setError] = useState('');
  const [now, setNow] = useState(Date.now());
  const [served, setServed] = useState<Version>();
  // A person's own change re-reads here without reopening the stream.
  const reload = useRef(() => {});
  useEffect(() => {
    let live = true;
    let reading = false, again = false;
    // One read at a time; changes during it ask for one more, so a burst never stacks reads or lands them out of order.
    const refresh = async () => {
      if (reading) return void (again = true);
      reading = true;
      do {
        again = false;
        await loadRows().then((r) => live && (setRows(r), setNow(Date.now()), setError('')), (e: Error) => live && setError(e.message));
      } while (again && live);
      reading = false;
    };
    let es: EventSource | undefined;
    let retry: ReturnType<typeof setTimeout> | undefined;
    const open = () => {
      es = new EventSource(WATCH);
      // Opened means subscribed, so a read now misses no write.
      es.onopen = () => void refresh();
      es.addEventListener('changed', () => void refresh());
      es.addEventListener('hello', (m) => setServed(JSON.parse(m.data) as Version));
      es.onerror = () => { if (es?.readyState === EventSource.CLOSED) (void probeSignIn(), (retry = setTimeout(open, REOPEN_MS))); };
    };
    reload.current = () => void refresh();
    void refresh();
    open();
    const clock = setInterval(() => setNow(Date.now()), TICK_MS);
    return () => {
      live = false;
      es?.close();
      clearTimeout(retry);
      clearInterval(clock);
    };
  }, []);
  return { rows, error, now, served, reload: () => reload.current() };
}

export type WorkspaceData = ReturnType<typeof useWorkspaceData>;

// A conversation with quack is headed by who you are talking to; its title only names it in lists.
function headerName(key: string, s: Parameters<typeof sessionTitle>[0]) {
  const t = sessionTitle(s);
  return key.startsWith('home:') ? { ...t, text: 'quack', full: 'quack' } : t;
}

// From lg the session list is the shell's sidebar and this is the main column; below lg the list is its own screen.
export function Workspace({ data, selected, home, list }: { data: WorkspaceData; selected?: { key: string; agent?: string }; home: boolean; list?: ReactNode }) {
  if (list && !selected && !home) return <>{list}</>;
  const row = data.rows?.find((r) => r.key === selected?.key);
  return (
    <div className="flex min-h-0 min-w-0 flex-col lg:h-dvh">
      {selected ? (
        <SessionView key={selected.key} sessionKey={selected.key} agent={selected.agent} spend={row?.spend} about={row && { agent: row.agent, created: row.createdAt, activity: row.lastActivity }} manage={row && { archived: row.state === 'archived', changed: data.reload }} name={headerName(selected.key, row ?? { key: selected.key, agent: selected.agent ?? '' })} />
      ) : (
        <div className="overflow-y-auto">
          <Home rows={data.rows?.filter((r) => r.state !== 'archived')} error={data.error} now={data.now} reload={data.reload} />
        </div>
      )}
    </div>
  );
}
