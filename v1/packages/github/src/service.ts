import { timingSafeEqual } from 'node:crypto';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { NoInstallation, type GithubApp } from './app.ts';
import { triggerFor, verifySignature } from './webhook.ts';

export type ServiceConfig = {
  webhookSecret: string;
  /** shortcut: one shared secret authenticates sessions to this service until workload tokens (A12) land. */
  serviceToken: string;
  distributorUrl: string;
  label: string;
  /** The agent that backs unattended triggers; it must be promoted (DEP-6). */
  agent: string;
};

/** Throws unless the distributor lists `agent` as promoted; the unattended trigger must not start on a draft. */
export async function assertPromoted(distributorUrl: string, agent: string): Promise<void> {
  let agents: Array<{ name: string; status: string }>;
  try {
    const res = await fetch(`${distributorUrl}/agents`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    agents = ((await res.json()) as { agents: typeof agents }).agents;
  } catch (err) {
    throw new Error(`cannot verify agent promotion: distributor at ${distributorUrl} unreachable or unhealthy (${String(err)})`);
  }
  const found = agents.find((a) => a.name === agent);
  if (!found) throw new Error(`agent ${agent} is not registered with the distributor`);
  if (found.status !== 'promoted') throw new Error(`agent ${agent} is ${found.status}; only a promoted agent can back the unattended GitHub trigger`);
}

const MAX_BODY = 5 << 20;
const EVENTS = new Set(['APPROVE', 'REQUEST_CHANGES', 'COMMENT']);

async function raw(req: IncomingMessage): Promise<Buffer> {
  let size = 0;
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    size += chunk.length;
    if (size > MAX_BODY) throw new Error('body too large');
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}

const send = (res: ServerResponse, status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));

function authorized(req: IncomingMessage, token: string): boolean {
  const given = Buffer.from(req.headers.authorization ?? '');
  const expected = Buffer.from(`Bearer ${token}`);
  return given.length === expected.length && timingSafeEqual(given, expected);
}

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

async function webhook(req: IncomingMessage, res: ServerResponse, app: GithubApp, config: ServiceConfig) {
  const body = await raw(req);
  if (!verifySignature(config.webhookSecret, body, req.headers['x-hub-signature-256'] as string | undefined)) return send(res, 401, { error: 'bad signature' });
  const trigger = triggerFor(req.headers['x-github-event'] as string | undefined, JSON.parse(body.toString('utf8')), config.label);
  if (!trigger) return send(res, 200, { ignored: true });
  // The delivery id is the request id, so a redelivery of the same event never starts a second run (HAR-5).
  const requestId = `github-delivery:${req.headers['x-github-delivery'] ?? ''}`;
  const target = `${config.distributorUrl}/sessions/${encodeURIComponent(trigger.sessionKey)}/invocations?agent=${encodeURIComponent(config.agent)}`;
  const upstream = await fetch(target, { method: 'POST', body: JSON.stringify({ input: trigger.input, requestId }) });
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
async function postReview(req: IncomingMessage, res: ServerResponse, app: GithubApp) {
  const r = JSON.parse((await raw(req)).toString('utf8')) as Record<string, any>;
  const ok = typeof r.owner === 'string' && typeof r.repo === 'string' && Number.isInteger(r.number) && EVENTS.has(r.event) && typeof r.body === 'string' && r.body.length > 0;
  if (!ok) return send(res, 400, { error: 'need owner, repo, integer number, event (APPROVE|REQUEST_CHANGES|COMMENT) and a non-empty body' });
  const path = `/repos/${r.owner}/${r.repo}/pulls/${r.number}/reviews`;
  const { json } = await app.asInstallation(r.owner, r.repo, 'POST', path, { event: r.event, body: r.body, ...(r.commitId ? { commit_id: r.commitId } : {}) });
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
async function pullRequest(res: ServerResponse, app: GithubApp, owner: string, repo: string, number: number) {
  const pr = (await app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/pulls/${number}`)).json;
  if (!pr) return send(res, 404, { error: 'no such pull request' });
  const checks = { check_runs: await checkRuns(app, owner, repo, pr.head.sha) };
  const closes = [...new Set([...String(pr.body ?? '').matchAll(CLOSES)].map((m) => Number(m[1])))];
  const issues = await Promise.all(closes.map((n) => app.asInstallation(owner, repo, 'GET', `/repos/${owner}/${repo}/issues/${n}`).then((r) => r.json)));
  send(res, 200, {
    owner, repo, number, title: pr.title, body: pr.body ?? '', author: pr.user?.login,
    headSha: pr.head.sha, headRef: pr.head.ref, baseRef: pr.base.ref,
    checks: (checks?.check_runs ?? []).map((c: any) => ({ name: c.name, status: c.status, conclusion: c.conclusion })),
    linkedIssues: issues.filter(Boolean).map((i: any) => ({ number: i.number, title: i.title, body: i.body ?? '' })),
  });
}

export function createGithubService(app: GithubApp, config: ServiceConfig) {
  return createServer(async (req, res) => {
    try {
      const path = new URL(req.url ?? '', 'http://x').pathname;
      if (req.method === 'POST' && path === '/webhook') return await webhook(req, res, app, config);
      if (!authorized(req, config.serviceToken)) return send(res, 401, { error: 'unauthorized' });
      if (req.method === 'GET' && path === '/git-credential') return await gitCredential(req, res, app);
      if (req.method === 'POST' && path === '/reviews') return await postReview(req, res, app);
      const pull = /^\/pulls\/([^/]+)\/([^/]+)\/(\d+)$/.exec(path);
      if (req.method === 'GET' && pull) return await pullRequest(res, app, pull[1], pull[2], Number(pull[3]));
      send(res, 404, { error: 'not found' });
    } catch (err) {
      const bad = err instanceof SyntaxError || (err as Error).message === 'body too large';
      send(res, bad ? 400 : 502, { error: (err as Error).message });
    }
  });
}
