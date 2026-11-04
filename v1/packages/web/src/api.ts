// Every call to the distributor, so its contract lives in one place.
export type SessionSummary = { key: string; agent: string; title?: string; archived?: boolean; running: boolean; active: boolean; note?: string; createdAt: string; lastActivity: string };
/** What a session has spent: tokens by kind, and `cost` in dollars at the deployment's cloud-equivalent prices (0 when unpriced). */
export type Spend = { cost: number; tokens: { input: number; output: number; cacheRead: number; cacheWrite: number } };
export type AgentSummary = { name: string; version: number; status: 'draft' | 'promoted'; model?: string; description?: string };
export type RubricCheck = { label?: string; weight?: number | 'gate' };
export type AgentRubric = { criteria: Array<{ id: string; checks: RubricCheck[] }> };
export type AgentDetail = { name: string; version: number; status: 'draft' | 'promoted'; definition: Record<string, unknown>; prompt: string; rubric?: AgentRubric; versions: Array<{ version: number; status: 'draft' | 'promoted' }> };
export type Approval = { id: string; tool: string; args: unknown; requestedAt: number };
export type Check = { criterion: string; id: string; label: string; weight: number | 'gate'; met: number | null; notes: string[]; scope?: string };
// Gates and positive checks fail when not fully met; a pitfall (negative weight) fails when it showed up. Not applicable never fails.
export const checkFailed = (c: Check) => c.met !== null && (c.weight === 'gate' || c.weight > 0 ? c.met < 1 : c.met > 0);
export type Verdict = { passed: boolean; /** The judge released it unpassed after its revision limit. */ limitReached?: boolean; gatesFailed: string[]; score: number; checks: Check[] };
export type ArtifactLink = { label: string; url: string };
export type SealedArtifact = { kind: string; version: number; hash: string; content: string; sealedAt: number; verdict: Verdict | null; links?: ArtifactLink[]; summary?: string; /** What the version was written against (a review's head commit). */ basis?: string; carriedFrom?: { sha: string; url?: string } };

export async function json<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const p = (await res.json().catch(() => null)) as { detail?: string; error?: string } | null;
    throw new Error(p?.detail ?? p?.error ?? `HTTP ${res.status}`);
  }
  return (await res.json()) as T;
}

// Paths are relative to the page so the bundle works under any prefix (/ locally, /v1/ behind the proxy).
const base = (key: string) => `sessions/${encodeURIComponent(key)}`;

export const listSessions = async () => (await json<{ sessions: SessionSummary[] }>(await fetch('sessions'))).sessions;
export const listAgents = async () => (await json<{ agents: AgentSummary[] }>(await fetch('agents'))).agents;
export const getAgent = async (name: string, version?: number) => json<AgentDetail>(await fetch(`agents/${encodeURIComponent(name)}${version === undefined ? '' : `?version=${version}`}`));
export const promoteAgent = async (name: string, version: number) =>
  void (await json(await fetch(`agents/${encodeURIComponent(name)}/promote`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ version }) })));
export const listApprovals = async (key: string) => (await json<{ approvals: Approval[] }>(await fetch(`${base(key)}/approvals`))).approvals;
// Verdicts sealed before checks carried a label have none; their id reads well enough.
const named = (a: SealedArtifact): SealedArtifact =>
  a.verdict ? { ...a, verdict: { ...a.verdict, checks: a.verdict.checks.map((c) => ({ ...c, label: c.label ?? c.id.replace(/_/g, ' ') })) } } : a;
export const listArtifacts = async (key: string) => (await json<{ artifacts: SealedArtifact[] }>(await fetch(`${base(key)}/artifacts`))).artifacts.map(named);
// Records a session's caller attached to its invocations (a PR's files and checks); empty when there are none.
export const getMeta = async (key: string) => (await json<{ meta: Array<Record<string, unknown>> }>(await fetch(`${base(key)}/meta`))).meta;
const housekeep = async (path: string, method: string, body: object) =>
  void (await json(await fetch(path, { method, headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) })));
export const renameSession = (key: string, title: string) => housekeep(base(key), 'PATCH', { title });
export const archiveSession = (key: string, archived: boolean) => housekeep(base(key), 'PATCH', { archived });
export const deleteSession = (key: string) => housekeep(`${base(key)}/delete`, 'POST', {});
/** Where the session downloads as one JSON file; a plain link, so the browser saves it. */
export const exportUrl = (key: string) => `${base(key)}/export`;
export const abortRun = async (key: string) => void (await json(await fetch(`${base(key)}/abort`, { method: 'POST' })));

export type IndexApproval = { session: string; id: string; tool: string; args?: unknown; at: string };
export type IndexArtifact = { session: string; kind: string; version: number; verdict: { passed: boolean } | null; links?: ArtifactLink[]; /** The kind's own one-word reading of the content (a review's verdict), when it has one. */ outcome?: string; summary?: string; sealedAt: string };

// The index answers 404 when it is off, and 503 when its database is down; either way callers fall back to per-session reads.
async function indexRead<T>(path: string, field: string): Promise<T[] | undefined> {
  const rows: T[] = [];
  for (let token = ''; ;) {
    const res = await fetch(`${path}${token && `${path.includes('?') ? '&' : '?'}pageToken=${token}`}`).catch(() => undefined);
    if (!res?.ok) return undefined;
    const page = (await res.json()) as Record<string, T[]> & { nextPageToken?: string };
    rows.push(...(page[field] ?? []));
    if (!page.nextPageToken) return rows;
    token = page.nextPageToken;
  }
}
/** A question an extension escalated to a person; `askedAt` is when it was asked. */
export type Question = { id: string; question: string; options: string[]; evidence: string | null; askedAt: number; header?: string | null; title?: string | null; detail?: string | null; optionDescriptions?: Record<string, string>; suggested?: number | null; freeText?: boolean };
export type IndexQuestion = Omit<Question, 'askedAt'> & { session: string; at: string };
export const indexQuestions = () => indexRead<IndexQuestion>('index/questions?state=pending', 'questions');
export const listQuestions = async (key: string) => (await json<{ questions: Question[] }>(await fetch(`${base(key)}/questions`))).questions;
export type Answer = { choice: string; note?: string } | { text: string };
export const answerQuestion = async (key: string, id: string, answer: Answer) =>
  void (await json(await fetch(`${base(key)}/questions/${encodeURIComponent(id)}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(answer) })));
export type Failure = { step: string; error: string };
export type IndexFailure = Failure & { session: string; at: string };
export const indexFailures = () => indexRead<IndexFailure>('index/failures', 'failures');
export const resumeRun = async (key: string) => void (await json(await fetch(`${base(key)}/resume`, { method: 'POST' })));
export const indexSessions = () => indexRead<{ key: string } & Spend>('index/sessions', 'sessions');
export const approvalOf = (a: IndexApproval): Approval => ({ id: a.id, tool: a.tool, args: a.args, requestedAt: Date.parse(a.at) });
/** A session's pending approvals from the index, which knows stopped sessions too; the session's own peek (empty while it is stopped) only when the index is off. */
export async function sessionApprovals(key: string): Promise<Approval[]> {
  const indexed = await indexApprovals();
  return indexed ? indexed.filter((a) => a.session === key).map(approvalOf) : listApprovals(key);
}
export const indexApprovals = () => indexRead<IndexApproval>('index/approvals?state=pending', 'approvals');
export const indexArtifacts = () => indexRead<IndexArtifact>('index/artifacts', 'artifacts');

export async function decideApproval(key: string, id: string, decision: 'approve' | 'reject', reason?: string) {
  const res = await fetch(`${base(key)}/approvals/${encodeURIComponent(id)}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ decision, ...(reason ? { reason } : {}) }),
  });
  // 409 means it is unknown or already decided: the caller's card is stale, not failed.
  if (res.status !== 409) await json(res);
}

// Starts the contracts agent on a description as the define_contract action does; the distributor creates the session on first use.
export async function startContract(description: string): Promise<string> {
  const id = crypto.randomUUID();
  const key = `contracts:${id}`;
  const title = draftTitle(description);
  await json(await fetch(`${base(key)}/invocations?agent=contracts`, { method: 'POST', body: JSON.stringify({ requestId: `ui-contract:${id}`, title, input: description }) }));
  return key;
}

// The distributor refuses a socket for a key it does not know, so a conversation's first message goes through invocations, which create it.
export const startSession = async (key: string, agent: string, input: string) =>
  void (await json(await fetch(`${base(key)}/invocations?agent=${encodeURIComponent(agent)}`, { method: 'POST', body: JSON.stringify({ input, requestId: `ui-start:${Date.now()}` }) })));

export function socketUrl(key: string, agent?: string) {
  const url = new URL(`${base(key)}/ws${agent ? `?agent=${encodeURIComponent(agent)}` : ''}`, location.href);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  return url.href;
}

export type Version = { commit: string | null; builtAt: string | null };
let version: Promise<Version | null> | undefined;
// One request per page load; a failure resolves to null so the UI shows nothing.
export const getVersion = () => (version ??= fetch('version').then((r) => json<Version>(r)).catch(() => null));
export const forgetVersion = () => { version = undefined; };

// A working title from the description's first clause, cut at a word; the spec's own title takes over once it exists.
export function draftTitle(description: string): string {
  const first = description.replace(/\s+/g, ' ').trim().split(/(?<=[^\s]):\s|\.\s|\s\(/)[0]!;
  return first.length <= 60 ? first : `${first.slice(0, 60).replace(/\s+\S*$/, '')}…`;
}
