import { useEffect, useState, type ReactNode } from 'react';
import { listAgents, listApprovals, listSessions, type AgentSummary } from './api.ts';
import { Landing } from './Landing.tsx';
import { group, titleOf, type Row } from './sessions.ts';
import { SessionView } from './SessionView.tsx';

const REFRESH_MS = 15_000;

// Approvals are asked of running sessions only: a request to an idle one would start its process.
async function loadRows(): Promise<Row[]> {
  const sessions = await listSessions();
  return Promise.all(sessions.map(async (s) => ({ ...s, approvals: s.running ? await listApprovals(s.key).catch(() => []) : [] })));
}

export function useWorkspaceData(selected: string | undefined) {
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

export type WorkspaceData = ReturnType<typeof useWorkspaceData>;

// From lg the session list is the shell's sidebar and this is the main column; below lg the list is its own screen.
export function Workspace({ data, selected, isNew, list }: { data: WorkspaceData; selected?: { key: string; agent?: string }; isNew: boolean; list?: ReactNode }) {
  const open = Boolean(selected) || isNew;
  if (list && !open) return <>{list}</>;
  const row = data.rows?.find((r) => r.key === selected?.key);
  return (
    <div className="flex min-h-0 min-w-0 flex-col lg:h-[calc(100dvh-2.5rem)]">
      {selected ? (
        <SessionView key={selected.key} sessionKey={selected.key} agent={selected.agent} title={row ? titleOf(row) : selected.key} />
      ) : (
        <div className="overflow-y-auto"><Landing agents={data.agents} error={data.agentsError} groups={group(data.rows ?? [])} /></div>
      )}
    </div>
  );
}
