// Every call to the distributor, so its contract lives in one place.
export type SessionSummary = { key: string; agent: string; title?: string; running: boolean; lastActivity: string };
export type AgentSummary = { name: string; version: number };
export type Approval = { id: string; tool: string; args: unknown; requestedAt: number };
export type Check = { criterion: string; id: string; met: number | null; notes: string[] };
export type Verdict = { passed: boolean; gatesFailed: string[]; score: number; checks: Check[] };
export type SealedArtifact = { kind: string; version: number; hash: string; content: string; sealedAt: number; verdict: Verdict | null };

async function json<T>(res: Response): Promise<T> {
  if (!res.ok) throw new Error(((await res.json().catch(() => null)) as { error?: string } | null)?.error ?? `HTTP ${res.status}`);
  return (await res.json()) as T;
}

// Paths are relative to the page so the bundle works under any prefix (/ locally, /v1/ behind the proxy).
const base = (key: string) => `sessions/${encodeURIComponent(key)}`;

export const listSessions = async () => (await json<{ sessions: SessionSummary[] }>(await fetch('sessions'))).sessions;
export const listAgents = async () => (await json<{ agents: AgentSummary[] }>(await fetch('agents'))).agents;
export const listApprovals = async (key: string) => (await json<{ approvals: Approval[] }>(await fetch(`${base(key)}/approvals`))).approvals;
export const listArtifacts = async (key: string) => (await json<{ artifacts: SealedArtifact[] }>(await fetch(`${base(key)}/artifacts`))).artifacts;
export const abortRun = async (key: string) => void (await json(await fetch(`${base(key)}/abort`, { method: 'POST' })));

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
  const url = new URL(`${base(key)}/ws${agent ? `?agent=${encodeURIComponent(agent)}` : ''}`, location.href);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  return url.href;
}
