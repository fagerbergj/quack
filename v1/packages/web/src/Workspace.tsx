import { useEffect, useState } from 'react';
import { listAgents, listApprovals, listSessions, type AgentSummary } from './api.ts';
import { Landing } from './Landing.tsx';
import { SessionList } from './SessionList.tsx';
import { group, titleOf, type Row } from './sessions.ts';
import { SessionView } from './SessionView.tsx';

const REFRESH_MS = 15_000;

// Approvals are asked of running sessions only: a request to an idle one would start its process.
async function loadRows(): Promise<Row[]> {
  const sessions = await listSessions();
  return Promise.all(sessions.map(async (s) => ({ ...s, approvals: s.running ? await listApprovals(s.key).catch(() => []) : [] })));
}

function useWorkspaceData(selected: string | undefined) {
  const [rows, setRows] = useState<Row[]>();
  const [agents, setAgents] = useState<AgentSummary[]>([]);
  const [error, setError] = useState('');
  const [agentsError, setAgentsError] = useState('');
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    let live = true;
    const refresh = () => loadRows().then((r) => live && (setRows(r), setNow(Date.now()), setError('')), (e: Error) => live && setError(e.message));
    void refresh();
    const timer = setInterval(refresh, REFRESH_MS);
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [selected]);
  useEffect(() => void listAgents().then(setAgents, (e: Error) => setAgentsError(e.message)), []);
  return { rows, agents, agentsError, error, now };
}

// Below lg the list and the main pane stack: the list alone at #/, the main pane alone once something is open.
export function Workspace({ selected, isNew }: { selected?: { key: string; agent?: string }; isNew: boolean }) {
  const { rows, agents, agentsError, error, now } = useWorkspaceData(selected?.key);
  const open = Boolean(selected) || isNew;
  const row = rows?.find((r) => r.key === selected?.key);
  return (
    <div className="grid grid-cols-1 lg:h-[calc(100dvh-2.5rem)] lg:grid-cols-[330px_minmax(0,1fr)] lg:grid-rows-[minmax(0,1fr)]">
      <SessionList rows={rows} selected={selected?.key} error={error} now={now} open={open} />
      <div className={`min-h-0 min-w-0 flex-col lg:flex ${open ? 'flex' : 'hidden'}`}>
        {selected ? (
          <SessionView key={selected.key} sessionKey={selected.key} agent={selected.agent} title={row ? titleOf(row) : selected.key} />
        ) : (
          <div className="overflow-y-auto"><Landing agents={agents} error={agentsError} groups={group(rows ?? [])} /></div>
        )}
      </div>
    </div>
  );
}
