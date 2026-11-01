// Every call to the distributor, so its contract lives in one place.
export type SessionSummary = { key: string; agent: string; running: boolean; lastActivity: string };
export type AgentSummary = { name: string; version: number };
export type Approval = { id: string; tool: string; args: unknown; requestedAt: number };

async function json<T>(res: Response): Promise<T> {
  if (!res.ok) throw new Error(((await res.json().catch(() => null)) as { error?: string } | null)?.error ?? `HTTP ${res.status}`);
  return (await res.json()) as T;
}

const base = (key: string) => `/sessions/${encodeURIComponent(key)}`;

export const listSessions = async () => (await json<{ sessions: SessionSummary[] }>(await fetch('/sessions'))).sessions;
export const listAgents = async () => (await json<{ agents: AgentSummary[] }>(await fetch('/agents'))).agents;
export const listApprovals = async (key: string) => (await json<{ approvals: Approval[] }>(await fetch(`${base(key)}/approvals`))).approvals;

// shortcut: no auth (tailnet only) and the approver is a fixed name, until identity lands.
export async function decideApproval(key: string, id: string, decision: 'approve' | 'reject') {
  await json(
    await fetch(`${base(key)}/approvals/${encodeURIComponent(id)}`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ decision, by: 'web' }),
    }),
  );
}

export function socketUrl(key: string, agent?: string) {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${location.host}${base(key)}/ws${agent ? `?agent=${encodeURIComponent(agent)}` : ''}`;
}
