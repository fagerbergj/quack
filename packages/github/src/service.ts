import type { IncomingMessage, ServerResponse } from 'node:http';
import { invocationRequest, sendProblem, serviceHttp, submitToSession, tooLarge, type App } from '@quack/sdk';
import { GithubError, NoInstallation, RateLimited, type GithubApp } from './app.ts';
import { allReviews, reviewFeedback } from './feedback.ts';
import { fixQueued, fixRefused, noticeFor, reviewQueued, reviewRefused, settled } from './notice.ts';
import { pullEventStream } from './events.ts';
import { MIRROR_PATH, pullOf, pullStates } from './pulls.ts';
import { waitingQueue, type WaitingStore } from './queue.ts';
import { sqliteWaiting } from './waiting-store.ts';
import { closedIssues, stripMentions, triggerFor, verifySignature, type Trigger } from './webhook.ts';

export type ServiceConfig = {
  webhookSecret: string;
  distributorUrl: string;
  /** Reaches the distributor; in the distributor's process it is an in-process call. */
  fetch?: typeof fetch;
  label: string;
  /** The agent that backs unattended triggers; it must be promoted (DEP-6). */
  agent: string;
  /** The agent that backs `/fix`; without it `/fix` is ignored, so a deployment opts in. */
  implementerAgent?: string;
  /** The branch `/implement` starts from; absent, the repository's default branch. */
  issueBase?: Record<string, string>;
  /** Where PR states persist; without it they live in memory and are re-read after a restart. */
  statesFile?: string;
  /** Where reviews and `/fix`es waiting for a busy session persist; without it they wait in memory and a restart drops them. */
  waiting?: WaitingStore;
  /** How often waiting work retries; default 15 s. */
  queuePollMs?: number;
};

const BACKOFF_CAP_MS = 30_000;
const pause = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

// Throws unless the distributor lists `agent` as promoted (no unattended draft). Unreachable is retried with backoff, since compose starts both together.
export async function assertPromoted(distributorUrl: string, agent: string, sleep: (ms: number) => Promise<void> = pause, doFetch: typeof fetch = fetch): Promise<void> {
  let agents: Array<{ name: string; status: string }> | undefined;
  for (let delay = 1_000; !agents; delay = Math.min(delay * 2, BACKOFF_CAP_MS)) {
    try {
      const res = await doFetch(`${distributorUrl}/agents`);
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

const { send, readBody: raw } = serviceHttp({ maxBody: 5 << 20 });
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
const baseFor = (bases: Record<string, string> | undefined, owner: string, repo: string) =>
  Object.entries(bases ?? {}).find(([r]) => r.toLowerCase() === `${owner}/${repo}`.toLowerCase())?.[1];

// Work still waits this long for a busy session before it is given up on.
const QUEUE_TTL_MS = 6 * 3600_000;

/** A review or `/fix` that starts a run: what the webhook submits, and what waits in the queue while the session is busy. */
type Queued = {
  sessionKey: string; agent: string; requestId: string; input: string; title: string; owner: string; repo: string; number: number; at: number;
  /** The opening-card data computed for the first submission. */ meta?: object;
  role: Trigger['role'];
};
type WorkQueue = ReturnType<typeof waitingQueue<Queued>>;

// One waiting review per PR, so a newer push or /review replaces an older one; a /fix waits beside it under its own key.
const queueKey = (q: Queued) => (q.role === 'reviewer' ? `${q.sessionKey} review` : q.sessionKey);
const submissionOf = (q: Queued) => ({ key: q.sessionKey, agent: q.agent, input: q.input, requestId: q.requestId, title: q.title, ...(q.meta && { meta: q.meta }) });
const unreachable = (err: unknown) => new Response(JSON.stringify({ detail: `the distributor is unreachable: ${String(err)}` }), { status: 503 });
const submit = (config: ServiceConfig, q: Queued) => submitToSession(config.distributorUrl, submissionOf(q), config.fetch).catch(unreachable);
const postedCount = (state: RunState, q: Queued) => state.posted.get(prKey(q.owner, q.repo, q.number)) ?? 0;

const midRun = (reason: string) => reason.includes('mid-run');
// A busy session or a distributor outage is worth waiting out; any other refusal never succeeds on retry.
const waits = (status: number, reason: string) => status >= 500 || (status === 409 && midRun(reason));
const refusedNote = (q: Queued, reason: string, issue?: boolean) => (q.role === 'reviewer' ? reviewRefused(reason) : fixRefused(reason, issue));
const comment = (app: GithubApp, q: { owner: string; repo: string; number: number }, body: string) =>
  app.asInstallation(q.owner, q.repo, 'POST', `/repos/${q.owner}/${q.repo}/issues/${q.number}/comments`, { body });
const problemOf = async (upstream: Response) => {
  const problem = (await upstream.json().catch(() => ({}))) as { detail?: string; error?: string };
  return problem.detail ?? problem.error ?? 'the distributor refused the run';
};

// Waits out a review run and says on the PR when it ended without a review. shortcut: a service restart drops the watch, so that run goes unannounced.
function watchRun(app: GithubApp, config: ServiceConfig, state: RunState, q: Queued, before: number) {
  if (state.watched.has(q.requestId)) return;
  state.watched.add(q.requestId);
  const { url, body } = invocationRequest(config.distributorUrl, submissionOf(q));
  void settled(url, { ...JSON.parse(body), wait: true }, config.fetch)
    .then((outcome) => noticeFor(outcome, postedCount(state, q) > before))
    .then(async (note) => { if (note) await comment(app, q, note); })
    .catch((err) => console.warn(`github: run watch for ${q.sessionKey} failed: ${String(err)}`));
}

// A refused start is answered where it was written, never dropped: a busy session or an outage queues it, anything else is said on the PR. Returns the webhook's status.
async function answerRefusal(app: GithubApp, t: Trigger, upstream: Response, queue: WorkQueue, queued: Queued): Promise<number> {
  const reason = await problemOf(upstream);
  // An issue's /implement mid-run is refused, not queued, as before.
  if (!waits(upstream.status, reason) || (t.issue && upstream.status === 409)) {
    await comment(app, queued, refusedNote(queued, reason, t.issue));
    return 200;
  }
  const prior = await queue.add(queueKey(queued), queued);
  // A replaced review was already announced; a new /fix instruction is worth saying again.
  const fresh = queued.role === 'reviewer' ? !prior : prior?.requestId !== queued.requestId;
  if (fresh && upstream.status === 409) await comment(app, queued, queued.role === 'reviewer' ? reviewQueued : fixQueued);
  return 202;
}

// One retry of queued work: done once it started or can never start. A session still busy, or a distributor outage, waits for the next try.
async function startQueued(app: GithubApp, config: ServiceConfig, state: RunState, q: Queued): Promise<boolean> {
  const before = postedCount(state, q);
  const upstream = await submit(config, q);
  if (upstream.ok && q.role === 'reviewer') watchRun(app, config, state, q, before);
  if (upstream.ok) return true;
  const reason = await problemOf(upstream);
  const waiting = waits(upstream.status, reason);
  if (waiting && Date.now() - q.at < QUEUE_TTL_MS) return false;
  await comment(app, q, refusedNote(q, waiting ? 'the running agent did not finish in 6 hours' : reason));
  return true;
}

type States = ReturnType<typeof pullStates>;

// A close or reopen only updates the PR's mirrored state; it never starts a run.
function recordState(states: States, event: string | undefined, p: Record<string, any>): boolean {
  const pr = p.pull_request;
  if (event !== 'pull_request' || !pr || !['closed', 'reopened'].includes(p.action)) return false;
  states.write(p.repository.full_name, pullOf(pr), p.action);
  return true;
}

async function webhook(req: IncomingMessage, res: ServerResponse, app: GithubApp, config: ServiceConfig, state: RunState, states: States, queue: WorkQueue) {
  const body = await raw(req);
  if (!verifySignature(config.webhookSecret, body, req.headers['x-hub-signature-256'] as string | undefined)) return send(res, 401, { error: 'bad signature' });
  const event = req.headers['x-github-event'] as string | undefined;
  const hook = JSON.parse(body.toString('utf8'));
  if (recordState(states, event, hook)) return send(res, 200, { recorded: true });
  const trigger = triggerFor(event, hook, config.label);
  const agent = trigger?.role === 'implementer' ? config.implementerAgent : config.agent;
  if (!trigger || !agent) return send(res, 200, { ignored: true });
  // The delivery id is the request id, so a redelivery of the same event never starts a second run.
  const requestId = `github-delivery:${req.headers['x-github-delivery'] ?? ''}`;
  const session = { key: trigger.sessionKey, agent };
  const meta = await invocationMeta(app, trigger.repo.owner, trigger.repo.name, trigger.number);
  const queued: Queued = { sessionKey: trigger.sessionKey, agent, requestId, input: trigger.input, title: trigger.title, owner: trigger.repo.owner, repo: trigger.repo.name, number: trigger.number, at: Date.now(), meta, role: trigger.role };
  const before = postedCount(state, queued);
  const upstream = await submit(config, queued);
  // The unreviewed-run note is about reviews; a fix run ends in its own approval.
  if (upstream.ok && trigger.role === 'reviewer') watchRun(app, config, state, queued, before);
  if (upstream.ok) return send(res, 202, { session: trigger.sessionKey, status: upstream.status });
  const status = await answerRefusal(app, trigger, upstream, queue, queued);
  send(res, status, { session: trigger.sessionKey, status: upstream.status, ...(status === 202 ? { queued: true } : { noticed: true }) });
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

const deliveryMark = (r: Record<string, any>) => (typeof r.deliveryId === 'string' && r.deliveryId ? `\n\n<!-- quack-delivery: ${encodeURIComponent(r.deliveryId)} -->` : '');

// The review a delivery already posted, found by the mark in its body: a retry after a lost answer or a crash posts nothing twice.
async function postedBefore(app: GithubApp, r: Record<string, any>, slug: string, mark: string): Promise<any> {
  if (!mark) return undefined;
  const reviews = await allReviews(app, r.owner, r.repo, `/repos/${r.owner}/${r.repo}/pulls/${r.number}`, Infinity);
  return reviews.find((v) => String(v.user?.login).toLowerCase() === `${slug}[bot]`.toLowerCase() && String(v.body ?? '').includes(mark.trim()));
}

// Posts a review the session sealed and checked; the session never holds an API token. A `deliveryId` makes a repeat return the first post.
async function postReview(req: IncomingMessage, res: ServerResponse, app: GithubApp, state: RunState) {
  const r = JSON.parse((await raw(req)).toString('utf8')) as Record<string, any>;
  const ok = typeof r.owner === 'string' && typeof r.repo === 'string' && Number.isInteger(r.number) && EVENTS.has(r.event) && typeof r.body === 'string' && r.body.length > 0;
  if (!ok) return send(res, 400, { error: 'need owner, repo, integer number, event (APPROVE|REQUEST_CHANGES|COMMENT) and a non-empty body' });
  const comments = Array.isArray(r.comments) ? r.comments.filter((c: any) => typeof c?.path === 'string' && Number.isInteger(c.line) && typeof c.body === 'string') : [];
  const slug = await app.slug();
  const path = `/repos/${r.owner}/${r.repo}/pulls/${r.number}/reviews`;
  const mark = deliveryMark(r);
  const key = prKey(r.owner, r.repo, r.number);
  const count = () => state.posted.set(key, (state.posted.get(key) ?? 0) + 1);
  const earlier = await postedBefore(app, r, slug, mark);
  if (earlier) return (count(), send(res, 200, { id: earlier.id, url: earlier.html_url }));
  // GitHub refuses an approve or request-changes from a pull request's author, and the implementer opens pull requests as this app.
  const own = r.event !== 'COMMENT' && String((await app.asInstallation(r.owner, r.repo, 'GET', `/repos/${r.owner}/${r.repo}/pulls/${r.number}`)).json?.user?.login).toLowerCase() === `${slug}[bot]`.toLowerCase();
  const lead = own ? `**Verdict: ${r.event === 'APPROVE' ? 'approve' : 'request changes'}.** GitHub does not take this verdict from a pull request's own author, so it is posted as a comment.\n\n` : '';
  const post = (body: string, inline: any[]) => app.asInstallation(r.owner, r.repo, 'POST', path, {
    event: own ? 'COMMENT' : r.event, body: stripMentions(lead + body, slug) + mark, ...(r.commitId ? { commit_id: r.commitId } : {}),
    ...(inline.length ? { comments: inline.map((c) => ({ path: c.path, line: c.line, side: 'RIGHT', body: stripMentions(c.body, slug) })) } : {}),
  });
  // GitHub answers 422 when an inline line is off the diff; only then does the review go as written, findings in the body. Outages are the caller's to retry.
  const { json } = await post(r.body, comments).catch((err) => {
    if (!comments.length || !(err instanceof GithubError) || err.status !== 422) throw err;
    return post(typeof r.fallbackBody === 'string' ? r.fallbackBody : r.body, []);
  });
  count();
  send(res, 201, { id: json?.id, url: json?.html_url });
}

// Issues a description says it closes ("closes #12", "fixes #3"): the promises a review checks.

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

// A deleted issue answers 410 (a missing one 404, which reads as no body): either is no linked issue, not a failed read.
const gone = (err: unknown) => {
  if (err instanceof GithubError && err.status === 410) return undefined;
  throw err;
};

/** What a review needs to know about a pull request, read with the app's token on the session's behalf. */
async function prFacts(app: GithubApp, owner: string, repo: string, number: number) {
  const pr = (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/pulls/${number}`)).json;
  if (!pr) return undefined;
  const checks = { check_runs: await checkRuns(app, owner, repo, pr.head.sha) };
  const closes = closedIssues(pr.body);
  const issues = await Promise.all(closes.map((n) => app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/issues/${n}`).then((r) => r.json, gone)));
  return {
    owner, repo, number, title: pr.title, body: pr.body ?? '', author: pr.user?.login,
    state: pr.merged ? 'merged' : pr.state === 'closed' ? 'closed' : 'open',
    headSha: pr.head.sha, headRef: pr.head.ref, baseRef: pr.base.ref, headRepo: pr.head.repo?.full_name ?? '',
    checks: (checks?.check_runs ?? []).map((c: any) => ({ name: c.name, status: c.status, conclusion: c.conclusion })),
    linkedIssues: issues.filter(Boolean).map((i: any) => ({ number: i.number, title: i.title, body: i.body ?? '' })),
  };
}

// An issue the session may implement: its text, labels and the branch to cut from.
async function issueRoute(res: ServerResponse, app: GithubApp, owner: string, repo: string, number: number, base?: string) {
  const issue = (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/issues/${number}`)).json;
  if (!issue || issue.pull_request) return send(res, 404, { error: 'no such issue' });
  const baseRef = base ?? (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}`)).json?.default_branch;
  send(res, 200, { owner, repo, number, title: issue.title, body: issue.body ?? '', author: issue.user?.login ?? null, state: issue.state, labels: (issue.labels ?? []).map((l: any) => (typeof l === 'string' ? l : l.name)), baseRef });
}

// Opens the pull request a session pushed a branch for, after its approval; the session never holds the token.
async function openPullRequest(req: IncomingMessage, res: ServerResponse, app: GithubApp) {
  const r = JSON.parse((await raw(req)).toString('utf8')) as Record<string, any>;
  if (!['owner', 'repo', 'head', 'base', 'title', 'body'].every((k) => typeof r[k] === 'string' && r[k])) return send(res, 400, { error: 'need owner, repo, head, base, title and body' });
  const slug = await app.slug();
  const { json } = await app.asInstallation(r.owner, r.repo, 'POST', `/repos/${r.owner}/${r.repo}/pulls`, { head: r.head, base: r.base, title: stripMentions(r.title, slug), body: stripMentions(r.body, slug) });
  send(res, 201, { number: json?.number, url: json?.html_url });
}

// Rewrites a pull request's title and body from the description its approver saw; the session never holds the token.
async function editPullRequest(req: IncomingMessage, res: ServerResponse, app: GithubApp, owner: string, repo: string, number: number) {
  const r = JSON.parse((await raw(req)).toString('utf8')) as Record<string, any>;
  if (typeof r.title !== 'string' || !r.title || typeof r.body !== 'string') return send(res, 400, { error: 'need a non-empty title and a body' });
  const slug = await app.slug();
  const { json } = await app.asInstallation(owner, repo, 'PATCH', `/repos/${owner}/${repo}/pulls/${number}`, { title: stripMentions(r.title, slug), body: stripMentions(r.body, slug) });
  // GitHub's 404 comes back as no body; a vanished pull request is not an edit.
  if (!json?.html_url) return send(res, 404, { error: 'no such pull request' });
  send(res, 200, { url: json.html_url });
}

// The live head and description alone: one read, for callers that only check whether the PR moved.
async function pullHead(res: ServerResponse, app: GithubApp, owner: string, repo: string, number: number) {
  const pr = (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/pulls/${number}`)).json;
  send(res, pr ? 200 : 404, pr ? { headSha: pr.head.sha, body: pr.body ?? '' } : { error: 'no such pull request' });
}

// /issues|pulls/{owner}/{repo}/{n}: GET reads either, PATCH edits a pull request, GET .../head reads a PR's head; false when the path or method is none of these.
async function itemRoute(req: IncomingMessage, res: ServerResponse, app: GithubApp, config: ServiceConfig, path: string): Promise<boolean> {
  const item = /^\/(issues|pulls)\/([^/]+)\/([^/]+)\/(\d+)(\/head)?$/.exec(path);
  if (!item) return false;
  const [, what, owner, repo, n, head] = item;
  if (head && (what !== 'pulls' || req.method !== 'GET')) return false;
  if (head) await pullHead(res, app, owner, repo, Number(n));
  else if (req.method === 'PATCH' && what === 'pulls') await editPullRequest(req, res, app, owner, repo, Number(n));
  else if (req.method !== 'GET') return false;
  else if (what === 'issues') await issueRoute(res, app, owner, repo, Number(n), baseFor(config.issueBase, owner, repo));
  else await pullRequest(res, app, owner, repo, Number(n));
  return true;
}

async function pullRequest(res: ServerResponse, app: GithubApp, owner: string, repo: string, number: number) {
  const facts = await prFacts(app, owner, repo, number);
  send(res, facts ? 200 : 404, facts ? { ...facts, feedback: await reviewFeedback(app, owner, repo, number, facts.headSha) } : { error: 'no such pull request' });
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

const githubDown = (err: unknown) => err instanceof RateLimited || (err instanceof GithubError && err.status >= 500);

// A GitHub outage answers with the stored copy, marked stale per RFC 9111 (Age, Warning 110), so a badge outlasts the blip.
function sendStale(res: ServerResponse, kept: { pull: unknown; age: number }, what: string, err: unknown) {
  console.warn(`github: serving ${what} from ${kept.age}s ago; GitHub failed: ${(err as Error).message}`);
  res.setHeader('age', String(kept.age));
  res.setHeader('warning', '110 - "Response is Stale"');
  send(res, 200, kept.pull);
}

// GitHub's "Get a pull request", cut to the Pull subset and served from the store while fresh.
async function mirrorPull(res: ServerResponse, app: GithubApp, states: States, [, owner, repo, n]: RegExpExecArray) {
  try {
    const pull = await states.read(app, owner, repo, Number(n));
    if (pull) send(res, 200, pull);
    else sendProblem(res, 404, `no pull request ${owner}/${repo}#${n}`);
  } catch (err) {
    const kept = githubDown(err) ? states.stored(owner, repo, Number(n)) : undefined;
    if (kept) return sendStale(res, kept, `${owner}/${repo}#${n}`, err);
    if (err instanceof RateLimited) res.setHeader('retry-after', String(Math.ceil(err.retryAfterMs / 1000)));
    sendProblem(res, err instanceof NoInstallation ? 404 : err instanceof RateLimited ? 503 : err instanceof GithubError ? 502 : 500, (err as Error).message);
  }
}

// By cause, so a caller retries only what can succeed: GitHub's refusals keep their status, its outages are a bad gateway, a rate limit is 503, a fault here is 500.
function failureStatus(err: unknown): number {
  if (err instanceof SyntaxError) return 400;
  if (err instanceof NoInstallation) return 404;
  if (err instanceof RateLimited) return 503;
  // A 401 is the app's own credential failing upstream, not the caller's.
  if (err instanceof GithubError) return err.status >= 500 || err.status === 401 ? 502 : err.status;
  return 500;
}

/** `resume` re-arms the work a restart found in the store; `stop` ends the retry timer; `sweep` retries waiting work now. */
export function createGithubService(app: GithubApp, config: ServiceConfig): App & { resume(): void; stop(): void; sweep(): Promise<void> } {
  const state: RunState = { posted: new Map(), watched: new Set() };
  const states = pullStates(config.statesFile);
  const events = pullEventStream(states.subscribe);
  const queue = waitingQueue<Queued>(config.waiting ?? sqliteWaiting(':memory:'), (_key, q) => startQueued(app, config, state, q), config.queuePollMs);
  const stop = () => { queue.stop(); events.stop(); };
  return { resume: queue.resume, stop, sweep: queue.sweep, handle: async (req, res) => {
    try {
      const path = new URL(req.url ?? '', 'http://x').pathname;
      if (req.method === 'POST' && path === '/webhook') return await webhook(req, res, app, config, state, states, queue);
      const get = req.method === 'GET' ? { '/git-credential': () => gitCredential(req, res, app), '/events': () => events.handle(req, res) }[path] : undefined;
      if (get) return await get();
      const mirror = req.method === 'GET' ? MIRROR_PATH.exec(path) : null;
      if (mirror) return await mirrorPull(res, app, states, mirror);
      const post = req.method === 'POST' ? { '/reviews': () => postReview(req, res, app, state), '/pulls': () => openPullRequest(req, res, app) }[path] : undefined;
      if (post) return await post();
      if (!(await itemRoute(req, res, app, config, path))) send(res, 404, { error: 'not found' });
    } catch (err) {
      if (tooLarge(err)) return sendProblem(res, 413, 'body too large');
      if (err instanceof RateLimited) res.setHeader('retry-after', String(Math.ceil(err.retryAfterMs / 1000)));
      send(res, failureStatus(err), { error: (err as Error).message });
    }
  } };
}
