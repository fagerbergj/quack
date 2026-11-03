import { useEffect, useState, type ReactNode } from 'react';
import { indexApprovals, indexArtifacts, indexFailures, indexQuestions, listAgents, listApprovals, listQuestions, listSessions, type Approval, type AgentSummary, type Question } from './api.ts';
import { Landing } from './Landing.tsx';
import { look, sessionTitle } from './kinds.ts';
import { group, multiScope, type Row } from './sessions.ts';
import { SessionView } from './SessionView.tsx';

const REFRESH_MS = 15_000;

// The index knows pending approvals of idle sessions too. Without it, only running sessions are asked: a request to an idle one would start its process.
async function loadRows(): Promise<Row[]> {
  const [sessions, indexed, asked, sealed, failed] = await Promise.all([listSessions(), indexApprovals(), indexQuestions(), indexArtifacts(), indexFailures()]);
  const approvalsBy = Map.groupBy(indexed ?? [], (a) => a.session);
  const questionsBy = Map.groupBy(asked ?? [], (q) => q.session);
  const failuresBy = Map.groupBy(failed ?? [], (f) => f.session);
  const artifacts = Map.groupBy(sealed ?? [], (a) => a.session);
  // A repo prefix only earns its space when reviews come from more than one repo.
  const multiRepo = multiScope(sessions.map((s) => s.key));
  return Promise.all(sessions.map(async (s) => ({
    ...s,
    multiRepo,
    artifacts: artifacts.get(s.key) ?? [],
    failures: failuresBy.get(s.key) ?? [],
    approvals: indexed ? (approvalsBy.get(s.key) ?? []).map((a): Approval => ({ id: a.id, tool: a.tool, args: undefined, requestedAt: a.at })) : s.running ? await listApprovals(s.key).catch(() => []) : [],
    questions: asked ? (questionsBy.get(s.key) ?? []).map((q): Question => ({ ...q, askedAt: q.at })) : s.running ? await listQuestions(s.key).catch(() => []) : [],
  })));
}

export function useWorkspaceData(selected: string | undefined) {
  const [rows, setRows] = useState<Row[]>();
  const [agents, setAgents] = useState<AgentSummary[]>([]);
  const [error, setError] = useState('');
  const [agentsError, setAgentsError] = useState('');
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
  }, [selected, tick]);
  useEffect(() => void listAgents().then(setAgents, (e: Error) => setAgentsError(e.message)), []);
  return { rows, agents, agentsError, error, now, reload: () => setTick((t) => t + 1) };
}

export type WorkspaceData = ReturnType<typeof useWorkspaceData>;

// From lg the session list is the shell's sidebar and this is the main column; below lg the list is its own screen.
export function Workspace({ data, selected, isNew, list }: { data: WorkspaceData; selected?: { key: string; agent?: string }; isNew: boolean; list?: ReactNode }) {
  const open = Boolean(selected) || isNew;
  if (list && !open) return <>{list}</>;
  const row = data.rows?.find((r) => r.key === selected?.key);
  const l = row && !row.active ? look(row) : undefined;
  return (
    <div className="flex min-h-0 min-w-0 flex-col lg:h-dvh">
      {selected ? (
        <SessionView key={selected.key} sessionKey={selected.key} agent={selected.agent} delivered={l?.delivered ? l : undefined} manage={row && { archived: Boolean(row.archived), changed: data.reload }} name={sessionTitle(row ?? { key: selected.key, agent: selected.agent ?? '', running: false, active: false, createdAt: '', lastActivity: '' })} />
      ) : (
        <div className="overflow-y-auto"><Landing agents={data.agents} error={data.agentsError} groups={group((data.rows ?? []).filter((r) => !r.archived))} /></div>
      )}
    </div>
  );
}
