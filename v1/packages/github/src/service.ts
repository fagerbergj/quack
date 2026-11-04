import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { invocationRequest, serviceHttp, submitToSession } from '@quack/sdk';
import { GithubError, NoInstallation, type GithubApp } from './app.ts';
import { fixRefused, noticeFor, settled } from './notice.ts';
import { prStateKey, pullStates, stateOf } from './pulls.ts';
import { stripMentions, triggerFor, verifySignature, type Trigger } from './webhook.ts';

export type ServiceConfig = {
  webhookSecret: string;
  /** shortcut: one shared secret authenticates sessions to this service until workload tokens (A12) land. */
  serviceToken: string;
  distributorUrl: string;
  label: string;
  /** The agent that backs unattended triggers; it must be promoted (DEP-6). */
  agent: string;
  /** The agent that backs `/fix`; without it `/fix` is ignored, so a deployment opts in. */
  implementerAgent?: string;
  /** Where PR states persist; without it they live in memory and are re-read after a restart. */
  statesFile?: string;
};

const BACKOFF_CAP_MS = 30_000;
const pause = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

// Throws unless the distributor lists `agent` as promoted (no unattended draft). Unreachable is retried with backoff, since compose starts both together.
export async function assertPromoted(distributorUrl: string, agent: string, sleep: (ms: number) => Promise<void> = pause): Promise<void> {
  let agents: Array<{ name: string; status: string }> | undefined;
  for (let delay = 1_000; !agents; delay = Math.min(delay * 2, BACKOFF_CAP_MS)) {
    try {
      const res = await fetch(`${distributorUrl}/agents`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      agents = ((await res.json()) as { agents: NonNullable<typeof agents> }).agents;
    } catch {
      await sleep(delay);
    }
  }
  const found = agents.find((a) => a.name === agent);
  if (!found) throw new Error(`agent ${agent} is not registered with the distributor`);
  if (found.status !== 'promoted') throw new Error(`agent ${agent} is ${found.status}; only a promoted agent can back the unattended GitHub trigger`);
}

const { send, authorized, readBody: raw } = serviceHttp({ maxBody: 5 << 20 });
const EVENTS = new Set(['APPROVE', 'REQUEST_CHANGES', 'COMMENT']);

/** owner/repo from a github.com URL such as https://github.com/acme/widgets(.git); undefined for any other host. */
export function ownerRepo(raw: string): { owner: string; repo: string } | undefined {
  try {
    const u = new URL(raw);
    const [owner, repo] = u.pathname.replace(/^\/+|\/+$/g, '').split('/');
    if (u.hostname.toLowerCase() !== 'github.com' || !owner || !repo) return undefined;
    return { owner, repo: repo.replace(/\.git$/, '') };
  } catch {
    return undefined;
  }
}

// Reviews posted per PR, and the runs already watched: a run is watched once, so no run notes twice.
type RunState = { posted: Map<string, number>; watched: Set<string> };
// Lowercased like the installation cache: the webhook and the session may case the same repository differently.
const prKey = (owner: string, repo: string, number: number) => `${owner.toLowerCase()}/${repo.toLowerCase()}#${number}`;

// Waits out a run the webhook started and says on the PR when it ended without a review. shortcut: a service restart drops the watch, so that run goes unannounced.
async function announceIfUnreviewed(app: GithubApp, state: RunState, t: Trigger, target: string, payload: object, before: number) {
  try {
    const outcome = await settled(target, { ...payload, wait: true });
    const note = noticeFor(outcome, (state.posted.get(prKey(t.repo.owner, t.repo.name, t.number)) ?? 0) > before);
    if (note) await app.asInstallation(t.repo.owner, t.repo.name, 'POST', `/repos/${t.repo.owner}/${t.repo.name}/issues/${t.number}/comments`, { body: note });
  } catch (err) {
    console.warn(`github: run watch for ${t.sessionKey} failed: ${String(err)}`);
  }
}

// A /fix or /implement the distributor refused is answered where it was written with its reason, never dropped.
async function noticeRefusedFix(app: GithubApp, t: Trigger, upstream: Response): Promise<boolean> {
  if (upstream.status !== 409 || t.role !== 'implementer') return false;
  const problem = (await upstream.json().catch(() => ({}))) as { detail?: string; error?: string };
  const reason = problem.detail ?? problem.error ?? 'the distributor refused the run';
  await app.asInstallation(t.repo.owner, t.repo.name, 'POST', `/repos/${t.repo.owner}/${t.repo.name}/issues/${t.number}/comments`, { body: fixRefused(reason, t.issue) });
  return true;
}

type States = ReturnType<typeof pullStates>;

// A close or reopen only updates the PR's recorded state; it never starts a run.
function recordState(states: States, event: string | undefined, p: Record<string, any>): boolean {
  const pr = p.pull_request;
  if (event !== 'pull_request' || !pr || !['closed', 'reopened'].includes(p.action)) return false;
  const at = pr.merged_at ?? pr.closed_at ?? pr.updated_at ?? new Date().toISOString();
  states.record(prStateKey(p.repository.owner.login, p.repository.name, pr.number), p.action === 'reopened' ? 'open' : stateOf(pr), at);
  return true;
}

async function queueFollowUp(res: ServerResponse, config: ServiceConfig, t: Trigger, session: { key: string; agent: string }, requestId: string) {
  const queued = await submitToSession(config.distributorUrl, { ...session, input: t.input, requestId, whenBusy: 'followUp' });
  send(res, queued.ok ? 202 : 502, { session: t.sessionKey, status: queued.status });
}

async function webhook(req: IncomingMessage, res: ServerResponse, app: GithubApp, config: ServiceConfig, state: RunState, states: States) {
  const body = await raw(req);
  if (!verifySignature(config.webhookSecret, body, req.headers['x-hub-signature-256'] as string | undefined)) return send(res, 401, { error: 'bad signature' });
  const event = req.headers['x-github-event'] as string | undefined;
  const hook = JSON.parse(body.toString('utf8'));
  if (recordState(states, event, hook)) return send(res, 200, { recorded: true });
  const trigger = triggerFor(event, hook, config.label, await app.slug());
  const agent = trigger?.role === 'implementer' ? config.implementerAgent : config.agent;
  if (!trigger || !agent) return send(res, 200, { ignored: true });
  // The delivery id is the request id, so a redelivery of the same event never starts a second run (HAR-5).
  const requestId = `github-delivery:${req.headers['x-github-delivery'] ?? ''}`;
  const session = { key: trigger.sessionKey, agent };
  if (trigger.followUp) return queueFollowUp(res, config, trigger, session, requestId);
  const meta = await invocationMeta(app, trigger.repo.owner, trigger.repo.name, trigger.number);
  const payload = { input: trigger.input, requestId, title: trigger.title, ...(meta && { meta }) };
  const before = state.posted.get(prKey(trigger.repo.owner, trigger.repo.name, trigger.number)) ?? 0;
  const upstream = await submitToSession(config.distributorUrl, { ...session, ...payload });
  if (await noticeRefusedFix(app, trigger, upstream)) return send(res, 200, { session: trigger.sessionKey, status: 409, noticed: true });
  // The unreviewed-run note is about reviews; a fix run ends in its own approval.
  if (upstream.ok && trigger.role === 'reviewer' && !state.watched.has(requestId)) {
    state.watched.add(requestId);
    void announceIfUnreviewed(app, state, trigger, invocationRequest(config.distributorUrl, { ...session, ...payload }).url, payload, before);
  }
  send(res, upstream.ok ? 202 : 502, { session: trigger.sessionKey, status: upstream.status });
}

async function gitCredential(req: IncomingMessage, res: ServerResponse, app: GithubApp) {
  const target = ownerRepo(new URL(req.url ?? '', 'http://x').searchParams.get('url') ?? '');
  if (!target) return send(res, 404, { error: 'not a github.com repository' });
  try {
    send(res, 200, { username: 'x-access-token', token: await app.tokenFor(target.owner, target.repo) });
  } catch (err) {
    send(res, err instanceof NoInstallation ? 404 : 502, { error: (err as Error).message });
  }
}

// Posts a review the session sealed and checked; the session never holds an API token (D56).
async function postReview(req: IncomingMessage, res: ServerResponse, app: GithubApp, state: RunState) {
  const r = JSON.parse((await raw(req)).toString('utf8')) as Record<string, any>;
  const ok = typeof r.owner === 'string' && typeof r.repo === 'string' && Number.isInteger(r.number) && EVENTS.has(r.event) && typeof r.body === 'string' && r.body.length > 0;
  if (!ok) return send(res, 400, { error: 'need owner, repo, integer number, event (APPROVE|REQUEST_CHANGES|COMMENT) and a non-empty body' });
  const comments = Array.isArray(r.comments) ? r.comments.filter((c: any) => typeof c?.path === 'string' && Number.isInteger(c.line) && typeof c.body === 'string') : [];
  const slug = await app.slug();
  const path = `/repos/${r.owner}/${r.repo}/pulls/${r.number}/reviews`;
  const post = (body: string, inline: any[]) => app.asInstallation(r.owner, r.repo, 'POST', path, {
    event: r.event, body: stripMentions(body, slug), ...(r.commitId ? { commit_id: r.commitId } : {}),
    ...(inline.length ? { comments: inline.map((c) => ({ path: c.path, line: c.line, side: 'RIGHT', body: stripMentions(c.body, slug) })) } : {}),
  });
  // GitHub answers 422 when an inline line is off the diff; only then does the review go as written, findings in the body. Outages are the caller's to retry.
  const { json } = await post(r.body, comments).catch((err) => {
    if (!comments.length || !(err instanceof GithubError) || err.status !== 422) throw err;
    return post(typeof r.fallbackBody === 'string' ? r.fallbackBody : r.body, []);
  });
  const key = prKey(r.owner, r.repo, r.number);
  state.posted.set(key, (state.posted.get(key) ?? 0) + 1);
  send(res, 201, { id: json?.id, url: json?.html_url });
}

const QUOTE_CHARS = 300;

// Answers a mention in its thread: a reply under a review comment, or a new comment quoting an issue comment. The session never holds the token (D56).
async function postReply(req: IncomingMessage, res: ServerResponse, app: GithubApp) {
  const r = JSON.parse((await raw(req)).toString('utf8')) as Record<string, any>;
  const ok = typeof r.owner === 'string' && typeof r.repo === 'string' && Number.isInteger(r.number) && Number.isInteger(r.commentId) && (r.kind === 'review' || r.kind === 'issue') && typeof r.body === 'string' && r.body.length > 0;
  if (!ok) return send(res, 400, { error: 'need owner, repo, integer number and commentId, kind (review|issue) and a non-empty body' });
  const slug = await app.slug();
  const base = `/repos/${r.owner}/${r.repo}`;
  if (r.kind === 'review') {
    const { json } = await app.asInstallation(r.owner, r.repo, 'POST', `${base}/pulls/${r.number}/comments/${r.commentId}/replies`, { body: stripMentions(r.body, slug) });
    return send(res, 201, { id: json?.id, url: json?.html_url });
  }
  const original = (await app.asInstallation(r.owner, r.repo, 'GET', `${base}/issues/comments/${r.commentId}`)).json;
  const quote = String(original?.body ?? '').slice(0, QUOTE_CHARS).split('\n').map((l) => `> ${l}`).join('\n');
  const { json } = await app.asInstallation(r.owner, r.repo, 'POST', `${base}/issues/${r.number}/comments`, { body: stripMentions(`${quote}\n\n${r.body}`, slug) });
  send(res, 201, { id: json?.id, url: json?.html_url });
}

// Issues a description says it closes ("closes #12", "fixes #3"): the promises a review checks.
const CLOSES = /\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)/gi;

const MAX_CHECK_PAGES = 10;

// Every check run, page by page: a failing check past the first page must still reach the verdict gate.
async function checkRuns(app: GithubApp, owner: string, repo: string, sha: string): Promise<any[]> {
  const runs: any[] = [];
  for (let page = 1; page <= MAX_CHECK_PAGES; page++) {
    const batch = (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/commits/${sha}/check-runs?per_page=100&page=${page}`)).json?.check_runs ?? [];
    runs.push(...batch);
    if (batch.length < 100) return runs;
  }
  throw new Error(`more than ${MAX_CHECK_PAGES * 100} check runs on ${sha}; refusing to review against a partial CI result`);
}

/** What a review needs to know about a pull request, read with the app's token on the session's behalf. */
async function prFacts(app: GithubApp, owner: string, repo: string, number: number) {
  const pr = (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/pulls/${number}`)).json;
  if (!pr) return undefined;
  const checks = { check_runs: await checkRuns(app, owner, repo, pr.head.sha) };
  const closes = [...new Set([...String(pr.body ?? '').matchAll(CLOSES)].map((m) => Number(m[1])))];
  const issues = await Promise.all(closes.map((n) => app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/issues/${n}`).then((r) => r.json)));
  return {
    owner, repo, number, title: pr.title, body: pr.body ?? '', author: pr.user?.login,
    state: pr.merged ? 'merged' : pr.state === 'closed' ? 'closed' : 'open',
    headSha: pr.head.sha, headRef: pr.head.ref, baseRef: pr.base.ref, headRepo: pr.head.repo?.full_name ?? '',
    checks: (checks?.check_runs ?? []).map((c: any) => ({ name: c.name, status: c.status, conclusion: c.conclusion })),
    linkedIssues: issues.filter(Boolean).map((i: any) => ({ number: i.number, title: i.title, body: i.body ?? '' })),
  };
}

// An issue the session may implement: its text, labels and the repository's default branch.
async function issueRoute(res: ServerResponse, app: GithubApp, owner: string, repo: string, number: number) {
  const issue = (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/issues/${number}`)).json;
  if (!issue || issue.pull_request) return send(res, 404, { error: 'no such issue' });
  const baseRef = (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}`)).json?.default_branch;
  send(res, 200, { owner, repo, number, title: issue.title, body: issue.body ?? '', author: issue.user?.login ?? null, state: issue.state, labels: (issue.labels ?? []).map((l: any) => (typeof l === 'string' ? l : l.name)), baseRef });
}

// Opens the pull request a session pushed a branch for, after its approval; the session never holds the token (D56).
async function openPullRequest(req: IncomingMessage, res: ServerResponse, app: GithubApp) {
  const r = JSON.parse((await raw(req)).toString('utf8')) as Record<string, any>;
  if (!['owner', 'repo', 'head', 'base', 'title', 'body'].every((k) => typeof r[k] === 'string' && r[k])) return send(res, 400, { error: 'need owner, repo, head, base, title and body' });
  const slug = await app.slug();
  const { json } = await app.asInstallation(r.owner, r.repo, 'POST', `/repos/${r.owner}/${r.repo}/pulls`, { head: r.head, base: r.base, title: stripMentions(r.title, slug), body: stripMentions(r.body, slug) });
  send(res, 201, { number: json?.number, url: json?.html_url });
}

async function pullRequest(res: ServerResponse, app: GithubApp, owner: string, repo: string, number: number) {
  const facts = await prFacts(app, owner, repo, number);
  send(res, facts ? 200 : 404, facts ?? { error: 'no such pull request' });
}

/** The opening card's data, sent beside the input and never in it. shortcut: the first 100 files; a longer diff shows its first page. */
export async function invocationMeta(app: GithubApp, owner: string, repo: string, number: number): Promise<object | undefined> {
  try {
    const facts = await prFacts(app, owner, repo, number);
    if (!facts) return undefined;
    const files = (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/pulls/${number}/files?per_page=100`)).json ?? [];
    return {
      id: facts.headSha, title: facts.title, body: facts.body, headSha: facts.headSha, headRef: facts.headRef, baseRef: facts.baseRef,
      files: files.map((f: any) => ({ path: f.filename, additions: f.additions, deletions: f.deletions })),
      checks: facts.checks,
      linked: facts.linkedIssues.map((i: any) => ({ number: i.number, title: i.title })),
    };
  } catch (err) {
    console.warn(`github: no opening-card data for ${owner}/${repo}#${number}: ${String(err)}`);
    return undefined;
  }
}

async function pullStatesRoute(req: IncomingMessage, res: ServerResponse, app: GithubApp, states: States) {
  const keys = (new URL(req.url ?? '', 'http://x').searchParams.get('keys') ?? '').split(',').filter(Boolean);
  send(res, 200, await states.lookup(app, keys));
}

export function createGithubService(app: GithubApp, config: ServiceConfig) {
  const state: RunState = { posted: new Map(), watched: new Set() };
  const states = pullStates(config.statesFile);
  return createServer(async (req, res) => {
    try {
      const path = new URL(req.url ?? '', 'http://x').pathname;
      if (req.method === 'POST' && path === '/webhook') return await webhook(req, res, app, config, state, states);
      if (!authorized(req, config.serviceToken)) return send(res, 401, { error: 'unauthorized' });
      const get = req.method === 'GET' ? { '/git-credential': () => gitCredential(req, res, app), '/apps/github/pulls': () => pullStatesRoute(req, res, app, states) }[path] : undefined;
      if (get) return await get();
      const post = req.method === 'POST' ? { '/reviews': () => postReview(req, res, app, state), '/replies': () => postReply(req, res, app), '/pulls': () => openPullRequest(req, res, app) }[path] : undefined;
      if (post) return await post();
      const item = req.method === 'GET' ? /^\/(issues|pulls)\/([^/]+)\/([^/]+)\/(\d+)$/.exec(path) : null;
      if (item) return await (item[1] === 'issues' ? issueRoute : pullRequest)(res, app, item[2], item[3], Number(item[4]));
      send(res, 404, { error: 'not found' });
    } catch (err) {
      const bad = err instanceof SyntaxError || (err as Error).message === 'body too large';
      send(res, bad ? 400 : 502, { error: (err as Error).message });
    }
  });
}
