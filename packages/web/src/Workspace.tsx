import { useEffect, useState, type ReactNode } from 'react';
import { listSessions } from './api.ts';
import { Home } from './Home.tsx';
import { sessionTitle } from './kinds.ts';
import { multiScope, rowOf, type Row } from './sessions.ts';
import { SessionView } from './SessionView.tsx';

const REFRESH_MS = 15_000;

// The row carries its pending-ask count and its last run, so the tick is this one list.
export async function loadRows(q?: string): Promise<Row[]> {
  // The server pages by last activity; the list stays in creation order so rows do not jump as sessions work.
  const sessions = (await listSessions(q)).sort((a, b) => b.createdAt.localeCompare(a.createdAt) || a.key.localeCompare(b.key));
  // A repo prefix only earns its space when reviews come from more than one repo.
  const multiRepo = multiScope(sessions.map((s) => s.key));
  return sessions.map((s) => rowOf(s, multiRepo));
}

export function useWorkspaceData() {
  const [rows, setRows] = useState<Row[]>();
  const [error, setError] = useState('');
  const [now, setNow] = useState(Date.now());
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let live = true;
    const refresh = () => loadRows().then((r) => live && (setRows(r), setNow(Date.now()), setError('')), (e: Error) => live && setError(e.message));
    void refresh();
    const timer = setInterval(refresh, REFRESH_MS);
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [tick]);
  return { rows, error, now, reload: () => setTick((t) => t + 1) };
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
