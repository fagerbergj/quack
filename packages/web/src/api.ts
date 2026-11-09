// Every call to the distributor, so its contract lives in one place.
export type Tokens = { input: number; output: number; cacheRead: number; cacheWrite: number };
/** What a session has spent: tokens by kind, and `cost` in dollars at the deployment's cloud-equivalent prices (0 when unpriced). */
export type Spend = { cost: number; tokens: Tokens };
/** Money as the Sessions API sends it: millionths of the unit as a decimal string. */
export type Money = { currencyCode: string; amountMicros: string };
export type Failure = { step: string; error: string };
export type SessionState = 'running' | 'idle' | 'stopped' | 'archived';
/** A session row of the catalog; dates are RFC 3339. */
export type Session = {
  key: string; agent: string; title?: string; state: SessionState; createdAt: string; lastActivity: string; cost: Money; tokens: Tokens;
  pendingAsks: number; oldestAskAt?: string; lastRun?: { outcome: 'succeeded' | 'denied' | 'failed' | 'aborted'; endedAt: string; failedSteps: Failure[] };
};
export const spendOf = (s: Pick<Session, 'cost' | 'tokens'>): Spend => ({ cost: Number(s.cost.amountMicros) / 1e6, tokens: s.tokens });
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
/** A sealed version as the session socket sends it; `verdict` is null while the judge works and absent when read over REST, which carries none. */
export type SealedArtifact = { kind: string; version: number; hash: string; content: string; sealedAt: number; verdict?: Verdict | null; links?: ArtifactLink[]; summary?: string; /** What the version was written against (a review's head commit). */ basis?: string; carriedFrom?: { sha: string; url?: string } };

export async function json<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const p = (await res.json().catch(() => null)) as { detail?: string; error?: string } | null;
    throw new Error(p?.detail ?? p?.error ?? `HTTP ${res.status}`);
  }
  return (await res.json()) as T;
}

// Paths are relative to the page so the bundle works under any prefix (/ locally, /v1/ behind the proxy).
const base = (key: string) => `sessions/${encodeURIComponent(key)}`;

/** Every row of a list matching `q` (Lucene, its terms built with escapeTerm): pages are followed to the end, so none is dropped. */
async function listAll<T>(path: string, field: string, q?: string): Promise<T[]> {
  const rows: T[] = [];
  for (let token = ''; ;) {
    const query = new URLSearchParams({ pageSize: '1000', ...(q && { q }), ...(token && { pageToken: token }) });
    const page = await json<Record<string, T[] | undefined> & { nextPageToken?: string }>(await fetch(`${path}?${query}`));
    rows.push(...(page[field] ?? []));
    if (!page.nextPageToken) return rows;
    token = page.nextPageToken;
  }
}
export const listSessions = (q?: string) => listAll<Session>('sessions', 'sessions', q);
export const listAgents = async () => (await json<{ agents: AgentSummary[] }>(await fetch('agents'))).agents;
export const getAgent = async (name: string, version?: number) => json<AgentDetail>(await fetch(`agents/${encodeURIComponent(name)}${version === undefined ? '' : `?version=${version}`}`));
export const promoteAgent = async (name: string, version: number) =>
  void (await json(await fetch(`agents/${encodeURIComponent(name)}/promote`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ version }) })));
// Verdicts sealed before checks carried a label have none; their id reads well enough.
const named = (a: SealedArtifact): SealedArtifact =>
  a.verdict ? { ...a, verdict: { ...a.verdict, checks: a.verdict.checks.map((c) => ({ ...c, label: c.label ?? c.id.replace(/_/g, ' ') })) } } : a;
/** One sealed version as the Sessions API lists it; `content` is in the per-session list only. */
export type Artifact = { session: string; kind: string; version: number; hash: string; content?: string; sealedAt: string; summary?: string; deliveries?: Array<{ where: string; link: string; at: string }> };
/** A session's sealed versions with their content, newest first. */
export const listArtifacts = async (key: string): Promise<SealedArtifact[]> =>
  (await listAll<Artifact>(`${base(key)}/artifacts`, 'artifacts')).map((a) => ({ kind: a.kind, version: a.version, hash: a.hash, content: a.content ?? '', sealedAt: Date.parse(a.sealedAt), summary: a.summary }));
/** Sealed versions across sessions, without content, newest first. */
export const listAllArtifacts = (q?: string) => listAll<Artifact>('artifacts', 'artifacts', q);
/** Sealed versions as the session socket sends them, with the judge's verdicts. */
export const sealedFrom = (artifacts: SealedArtifact[]) => artifacts.map(named);
// Records a session's caller attached to its invocations (a PR's files and checks); empty when there are none.
export const getMeta = async (key: string) => (await json<{ meta: Array<Record<string, unknown>> }>(await fetch(`${base(key)}/meta`))).meta;
const housekeep = async (path: string, method: string, body: object) =>
  void (await json(await fetch(path, { method, headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) })));
export const renameSession = (key: string, title: string) => housekeep(base(key), 'PATCH', { title });
// Unarchiving returns the session to stopped; the server refuses to archive a running one.
export const archiveSession = (key: string, archived: boolean) => housekeep(base(key), 'PATCH', { state: archived ? 'archived' : 'stopped' });
export const deleteSession = async (key: string) => void (await json(await fetch(base(key), { method: 'DELETE' })));
/** Where the session downloads as one JSON file; a plain link, so the browser saves it. */
export const exportUrl = (key: string) => `${base(key)}/export`;
export const abortRun = async (key: string) => void (await json(await fetch(`${base(key)}/abort`, { method: 'POST' })));
export const resumeRun = async (key: string) => void (await json(await fetch(`${base(key)}/resume`, { method: 'POST' })));

/** A question an extension escalated to a person; `askedAt` is when it was asked. */
export type Question = { id: string; question: string; options: string[]; evidence: string | null; askedAt: number; header?: string | null; title?: string | null; detail?: string | null; optionDescriptions?: Record<string, string>; suggested?: number | null; freeText?: boolean };
type Prompt = Omit<Question, 'id' | 'askedAt' | 'evidence' | 'freeText'> & { evidence?: string | null; freeText: boolean };
/** An approval or a question of one session, as the Sessions API lists them. */
export type Ask = {
  session: string; id: string; type: 'approval' | 'question'; state: 'pending' | 'resolved' | 'cancelled'; createdAt: string; resolvedAt?: string;
  resolution?: { by: { kind: 'person'; id: string } | { kind: 'tailnet' }; choice: string; text?: string; reason?: string };
  toolCall?: { tool: string; args: unknown }; prompt?: Prompt;
};
/** Asks across sessions, oldest first; `state:pending` is everything waiting on a person. */
export const listAsks = (q: string) => listAll<Ask>('asks', 'asks', q);
export const approvalOf = (a: Ask): Approval => ({ id: a.id, tool: a.toolCall?.tool ?? '', args: a.toolCall?.args, requestedAt: Date.parse(a.createdAt) });
export const questionOf = (a: Ask): Question => ({ question: '', options: [], ...a.prompt, evidence: a.prompt?.evidence ?? null, id: a.id, askedAt: Date.parse(a.createdAt) });

/** A question's choice is an option's label, or the free text itself; an approval's is approve or reject. */
export type Resolution = { choice: string; text?: string; reason?: string };
const resolveAsk = (key: string, id: string, resolution: Resolution) =>
  fetch(`${base(key)}/asks/${encodeURIComponent(id)}`, { method: 'PATCH', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ resolution }) });
export const answerQuestion = async (key: string, id: string, resolution: Resolution) => void (await json(await resolveAsk(key, id, resolution)));

/** False when the session answers 409: the approval is already decided or cancelled, so the caller's view of it is stale. */
export async function decideApproval(key: string, id: string, decision: 'approve' | 'reject', reason?: string): Promise<boolean> {
  const res = await resolveAsk(key, id, { choice: decision, ...(reason ? { reason } : {}) });
  if (res.status === 409) return false;
  await json(res);
  return true;
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
// The build the server runs now, past any cache; getVersion stays the build this page loaded with.
export const servedVersion = () => fetch('version', { cache: 'no-store' }).then((r) => json<Version>(r)).catch(() => null);

// A working title from the description's first clause, cut at a word; the spec's own title takes over once it exists.
export function draftTitle(description: string): string {
  const first = description.replace(/\s+/g, ' ').trim().split(/(?<=[^\s]):\s|\.\s|\s\(/)[0]!;
  return first.length <= 60 ? first : `${first.slice(0, 60).replace(/\s+\S*$/, '')}…`;
}
