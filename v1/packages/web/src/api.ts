// Every call to the distributor, so its contract lives in one place.
export type SessionSummary = { key: string; agent: string; title?: string; running: boolean; active: boolean; note?: string; createdAt: string; lastActivity: string };
export type AgentSummary = { name: string; version: number };
export type Approval = { id: string; tool: string; args: unknown; requestedAt: number };
export type Check = { criterion: string; id: string; label: string; weight: number | 'gate'; met: number | null; notes: string[] };
// Gates and positive checks fail when not fully met; a pitfall (negative weight) fails when it showed up. Not applicable never fails.
export const checkFailed = (c: Check) => c.met !== null && (c.weight === 'gate' || c.weight > 0 ? c.met < 1 : c.met > 0);
export type Verdict = { passed: boolean; gatesFailed: string[]; score: number; checks: Check[] };
export type ArtifactLink = { label: string; url: string };
export type SealedArtifact = { kind: string; version: number; hash: string; content: string; sealedAt: number; verdict: Verdict | null; links?: ArtifactLink[] };

export async function json<T>(res: Response): Promise<T> {
  if (!res.ok) throw new Error(((await res.json().catch(() => null)) as { error?: string } | null)?.error ?? `HTTP ${res.status}`);
  return (await res.json()) as T;
}

// Paths are relative to the page so the bundle works under any prefix (/ locally, /v1/ behind the proxy).
const base = (key: string) => `sessions/${encodeURIComponent(key)}`;

export const listSessions = async () => (await json<{ sessions: SessionSummary[] }>(await fetch('sessions'))).sessions;
export const listAgents = async () => (await json<{ agents: AgentSummary[] }>(await fetch('agents'))).agents;
export const listApprovals = async (key: string) => (await json<{ approvals: Approval[] }>(await fetch(`${base(key)}/approvals`))).approvals;
// Verdicts sealed before checks carried a label have none; their id reads well enough.
const named = (a: SealedArtifact): SealedArtifact =>
  a.verdict ? { ...a, verdict: { ...a.verdict, checks: a.verdict.checks.map((c) => ({ ...c, label: c.label ?? c.id.replace(/_/g, ' ') })) } } : a;
export const listArtifacts = async (key: string) => (await json<{ artifacts: SealedArtifact[] }>(await fetch(`${base(key)}/artifacts`))).artifacts.map(named);
// Records a session's caller attached to its invocations (a PR's files and checks); empty when there are none.
export const getMeta = async (key: string) => (await json<{ meta: Array<Record<string, unknown>> }>(await fetch(`${base(key)}/meta`))).meta;
export const abortRun = async (key: string) => void (await json(await fetch(`${base(key)}/abort`, { method: 'POST' })));

export type IndexApproval = { session: string; id: string; tool: string; at: number };
export type IndexArtifact = { session: string; kind: string; version: number; verdict: { passed: boolean } | null; links?: ArtifactLink[]; /** The kind's own one-word reading of the content (a review's verdict), when it has one. */ outcome?: string; sealedAt: number };

// The index answers 404 when it is off, and 503 when its database is down; either way callers fall back to per-session reads.
async function indexRead<T>(path: string, field: string): Promise<T[] | undefined> {
  const res = await fetch(path).catch(() => undefined);
  return res?.ok ? ((await res.json()) as Record<string, T[]>)[field] : undefined;
}
/** A question an extension escalated to a person; `askedAt` is when it was asked. */
export type Question = { id: string; question: string; options: string[]; evidence: string | null; askedAt: number };
export type IndexQuestion = Omit<Question, 'askedAt'> & { session: string; at: number };
export const indexQuestions = () => indexRead<IndexQuestion>('index/questions?state=pending', 'questions');
export const listQuestions = async (key: string) => (await json<{ questions: Question[] }>(await fetch(`${base(key)}/questions`))).questions;
export const answerQuestion = async (key: string, id: string, choice: string) =>
  void (await json(await fetch(`${base(key)}/questions/${encodeURIComponent(id)}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ choice }) })));
export type Failure = { step: string; error: string };
export type IndexFailure = Failure & { session: string; at: number };
export const indexFailures = () => indexRead<IndexFailure>('index/failures', 'failures');
export const resumeRun = async (key: string) => void (await json(await fetch(`${base(key)}/resume`, { method: 'POST' })));
export const indexApprovals = () => indexRead<IndexApproval>('index/approvals?state=pending', 'approvals');
export const indexArtifacts = () => indexRead<IndexArtifact>('index/artifacts', 'artifacts');

export async function decideApproval(key: string, id: string, decision: 'approve' | 'reject', reason?: string) {
  await json(
    await fetch(`${base(key)}/approvals/${encodeURIComponent(id)}`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ decision, ...(reason ? { reason } : {}) }),
    }),
  );
}

export function socketUrl(key: string, agent?: string) {
  const url = new URL(`${base(key)}/ws${agent ? `?agent=${encodeURIComponent(agent)}` : ''}`, location.href);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  return url.href;
}
