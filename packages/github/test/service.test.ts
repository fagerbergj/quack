import assert from 'node:assert/strict';
import { createHmac, createVerify, generateKeyPairSync } from 'node:crypto';
import { createServer, STATUS_CODES, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import { serveApp } from '@quack/sdk';
import { assertPromoted, createGithubService, githubApp, ownerRepo } from '../src/index.ts';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fixQueued, fixRefused, fixSteered, noticeFor, reviewQueued, reviewRefused } from '../src/notice.ts';
import { steerInput } from '../src/webhook.ts';
import { sqliteWaiting } from '../src/waiting-store.ts';
import type { GithubApp } from '../src/app.ts';

const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048, privateKeyEncoding: { type: 'pkcs8', format: 'pem' }, publicKeyEncoding: { type: 'spki', format: 'pem' } });
const servers: Server[] = [];
after(() => servers.forEach((s) => s.close()));

async function listen(server: Server) {
  servers.push(server.listen(0));
  await new Promise((r) => server.once('listening', r));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

// A fake GitHub API that verifies the app JWT the way GitHub does: RS256 over header.body, issuer = app id.
const calls: Array<{ method: string; path: string; auth: string; version?: string; body: any }> = [];
const commented: string[] = [];
const review = (id: number, login: string, state: string, body = '', commit_id = 'abc1230000000000000000000000000000000000', node_id?: string) => ({ id, user: { login }, state, body, commit_id, ...(node_id ? { node_id } : {}), submitted_at: new Date(Date.UTC(2026, 0, 1, 0, 0, id)).toISOString() });
// Out of order on purpose: pat's approval is listed before the request it supersedes. 100 older filler reviews push the rest onto page 2.
const reviews = [
  ...Array.from({ length: 100 }, (_, i) => review(i - 200, `filler${i}`, 'COMMENTED')),
  review(3, 'pat', 'APPROVED'),
  review(1, 'maria', 'CHANGES_REQUESTED', 'Handle the empty list.', 'old000'),
  review(2, 'pat', 'CHANGES_REQUESTED', 'Rename this.'),
  review(4, 'lee', 'COMMENTED', 'Nit: spacing.'),
  review(5, 'sam', 'COMMENTED'),
  review(6, 'quack-app[bot]', 'DISMISSED', 'stale'),
];
let graphqlErrors = false;
let pr7State = 'open';
let pr7Merged = false;
let scopedLifeMs = 3600_000;
let reviewCommentsDown = false;
const posted15: any[] = [];
const thread = (isResolved: boolean, path: string, body: string) => ({ isResolved, path, line: 4, originalLine: 4, comments: { nodes: [{ body, author: { login: 'maria' } }] } });
const threads = [thread(false, 'app.ts', 'Off by one.'), thread(true, 'old.ts', 'Fixed already.')];
const commentWaiters: Array<() => void> = [];
// Newest first, as the service asks: page 1 is full of others' comments and the reply, page 2 holds the app's comment it answers.
const reviewComments = [
  { id: 900, in_reply_to_id: 800, user: { login: 'dev' }, path: 'app.ts', line: 4, original_commit_id: 'abc1230000000000000000000000000000000000', created_at: '2026-11-09T23:01:00Z', body: 'Checked on prod.' },
  ...Array.from({ length: 99 }, (_, i) => ({ id: 500 + i, user: { login: 'maria' }, path: 'app.ts', line: 1, created_at: '2026-11-09T23:00:00Z', body: 'noise' })),
  { id: 800, user: { login: 'quack-app[bot]' }, path: 'app.ts', line: 4, original_commit_id: 'old999', created_at: '2026-11-09T22:58:00Z', body: 'question: exposed?' },
];
function jwtValid(auth: string) {
  const [head, body, sig] = auth.replace(/^Bearer /, '').split('.');
  const ok = createVerify('RSA-SHA256').update(`${head}.${body}`).verify(publicKey, Buffer.from(sig, 'base64url'));
  return ok && JSON.parse(Buffer.from(body, 'base64url').toString()).iss === '123';
}
const github = await listen(
  createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    const auth = String(req.headers.authorization);
    calls.push({ method: req.method!, path: req.url!, auth, version: req.headers['x-github-api-version'] as string | undefined, body: raw ? JSON.parse(raw) : undefined });
    const reply = (status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
    // GitHub's API is case-insensitive: a mixed-case owner reaches the same repository.
    const url = req.url!.replace(/^\/repos\/ACME\//, '/repos/acme/');
    if (url === '/app') return jwtValid(auth) ? reply(200, { slug: 'quack-app' }) : reply(401, {});
    if (url === '/repos/acme/widgets/installation') return jwtValid(auth) ? reply(200, { id: 42 }) : reply(401, {});
    if (url === '/repos/acme/other/installation') return jwtValid(auth) ? reply(200, { id: 42 }) : reply(401, {});
    if (url === '/repos/acme/private/installation') return reply(404, { message: 'Not Found' });
    // A request that names repositories is a session's credential, minted as `ghs_scoped_<repo>`; the service's own token has no body.
    if (url === '/app/installations/42/access_tokens') {
      const scope = calls.at(-1)!.body?.repositories?.[0];
      return jwtValid(auth) ? reply(201, { token: scope ? `ghs_scoped_${scope}` : 'ghs_test', expires_at: new Date(Date.now() + (scope ? scopedLifeMs : 3600_000)).toISOString() }) : reply(401, {});
    }
    if (url === '/repos/acme/widgets/pulls/7' && req.method === 'PATCH') return reply(200, { number: 7, html_url: 'https://github.com/acme/widgets/pull/7' });
    if (url === '/repos/acme/widgets/pulls/404' && req.method === 'PATCH') return reply(404, { message: 'Not Found' });
    if (url === '/repos/acme/widgets/pulls/7' && auth === 'token ghs_test') return reply(200, { state: pr7State, merged: pr7Merged,number: 7, title: 't', body: 'Fixes #3', user: { login: 'dev' }, head: { sha: 'abc1230000000000000000000000000000000000', ref: 'fix' }, base: { ref: 'main' } });
    const mirrored = { html_url: `https://github.com/acme/widgets/pull/${url.split('/').at(-1)}`, title: 'Fix the cache', base: { repo: { full_name: 'acme/widgets' } } };
    if (url === '/repos/acme/widgets/pulls/9') return reply(200, { ...mirrored, number: 9, state: 'closed', merged: true, merged_at: '2026-11-01T00:00:00Z', closed_at: '2026-11-01T00:00:00Z' });
    if (url === '/repos/acme/widgets/pulls/10') return reply(200, { ...mirrored, number: 10, state: 'closed', merged: false, merged_at: null, closed_at: '2026-11-02T00:00:00Z' });
    if (url === '/repos/acme/widgets/pulls/11') return reply(200, { ...mirrored, number: 11, state: 'open', merged: false, merged_at: null, closed_at: null, updated_at: '2026-11-03T00:00:00Z' });
    if (url === '/repos/acme/widgets/pulls/19') return reply(500, { message: 'Server Error' });
    if (url === '/repos/acme/widgets/pulls/24') return res.writeHead(429, { 'retry-after': '3600' }).end('{"message":"rate limited"}');
    if (url === '/repos/acme/widgets/pulls/25') return reply(503, { message: 'unavailable' });
    if (url === '/repos/acme/widgets/pulls/26') return res.writeHead(429, { 'retry-after': '3600' }).end('{"message":"rate limited"}');
    if (url === '/repos/acme/widgets/pulls/8' && auth === 'token ghs_test') return reply(200, { number: 8, title: 't', body: '', user: { login: 'dev' }, head: { sha: 'full111', ref: 'big' }, base: { ref: 'main' } });
    // Every page full: a commit whose check runs never end.
    if (url?.startsWith('/repos/acme/widgets/commits/full111/check-runs')) return reply(200, { check_runs: Array.from({ length: 100 }, (_, i) => ({ name: `c${i}`, status: 'completed', conclusion: 'success' })) });
    if (url === '/repos/acme/widgets/issues/12' && auth === 'token ghs_test') return reply(200, { number: 12, title: 'Add TTL', body: 'b', state: 'open', user: { login: 'dev' }, labels: [{ name: 'bug' }] });
    if (url === '/repos/acme/widgets/issues/9') return reply(200, { number: 9, pull_request: {} });
    if (url === '/repos/acme/widgets' && auth === 'token ghs_test') return reply(200, { default_branch: 'trunk' });
    if (url === '/repos/acme/widgets/pulls' && req.method === 'POST') return reply(201, { number: 13, html_url: 'https://github.com/acme/widgets/pull/13' });
    if (url === '/repos/acme/widgets/issues/12/comments') {
      commented.push(calls.at(-1)!.body.body);
      commentWaiters.splice(0).forEach((f) => f());
      return reply(201, {});
    }
    if (url === '/repos/acme/widgets/issues/7/comments') {
      commented.push(calls.at(-1)!.body.body);
      commentWaiters.splice(0).forEach((f) => f());
      return reply(201, {});
    }
    if (url === '/repos/acme/widgets/pulls/7/files?per_page=100') return reply(200, [{ filename: 'src/a.ts', additions: 5, deletions: 2 }]);
    if (url === '/repos/acme/widgets/issues/3') return reply(200, { number: 3, title: 'bug', body: 'b' });
    const page = /^\/repos\/acme\/widgets\/commits\/abc1230000000000000000000000000000000000\/check-runs\?per_page=100&page=(\d+)$/.exec(url);
    // 150 check runs: the only failing one is on the second page.
    if (page) return reply(200, { check_runs: Array.from({ length: page[1] === '1' ? 100 : 50 }, (_, i) => ({ name: `c${page[1]}-${i}`, status: 'completed', conclusion: page[1] === '2' && i === 49 ? 'failure' : 'success' })) });
    // GitHub refuses a whole review when one inline line is off the diff.
    if (url === '/repos/acme/widgets/pulls/7/reviews' && calls.at(-1)!.body?.comments?.some((c: any) => c.path === 'gone.ts')) return reply(422, { message: 'Line could not be resolved' });
    if (url === '/repos/acme/widgets/pulls/7/reviews' && calls.at(-1)!.body?.comments?.some((c: any) => c.path === 'down.ts')) return reply(503, { message: 'unavailable' });
    if (url === '/repos/acme/widgets/pulls/7/reviews') return auth === 'token ghs_test' ? reply(200, { id: 9, html_url: 'https://github.com/acme/widgets/pull/7#review-9' }) : reply(401, {});
    if (url === '/repos/acme/widgets/pulls/14') return reply(200, { number: 14, user: { login: 'Quack-App[bot]' }, head: { sha: 'abc1230000000000000000000000000000000000' } });
    if (url === '/repos/acme/widgets/pulls/14/reviews') return calls.at(-1)!.body.event === 'COMMENT' ? reply(200, { id: 10, html_url: 'https://github.com/acme/widgets/pull/14#review-10' }) : reply(422, { message: 'Can not approve your own pull request' });
    const reviewPage = /^\/repos\/acme\/widgets\/pulls\/7\/reviews\?per_page=100&page=(\d+)$/.exec(url);
    if (reviewPage) return reply(200, reviews.slice((Number(reviewPage[1]) - 1) * 100, Number(reviewPage[1]) * 100));
    const commentPage = /^\/repos\/acme\/widgets\/pulls\/7\/comments\?sort=created&direction=desc&per_page=100&page=(\d+)$/.exec(url);
    if (commentPage && reviewCommentsDown) return reply(503, { message: 'unavailable' });
    if (commentPage && auth === 'token ghs_test') return reply(200, reviewComments.slice((Number(commentPage[1]) - 1) * 100, Number(commentPage[1]) * 100));
    if (url === '/repos/acme/widgets/pulls/15' && req.method === 'GET') return reply(200, { number: 15, user: { login: 'dev' }, head: { sha: 'abc1230000000000000000000000000000000000' } });
    if (url === '/repos/acme/widgets/pulls/15/reviews' && req.method === 'POST') {
      const id = 100 + posted15.length;
      posted15.push({ id, user: { login: 'quack-app[bot]' }, state: 'COMMENTED', body: calls.at(-1)!.body.body, commit_id: calls.at(-1)!.body.commit_id, html_url: `https://github.com/acme/widgets/pull/15#review-${id}` });
      return reply(200, posted15.at(-1));
    }
    const page15 = /^\/repos\/acme\/widgets\/pulls\/15\/reviews\?per_page=100&page=(\d+)$/.exec(url);
    if (page15) return reply(200, [...reviews.slice(0, 100), ...posted15].slice((Number(page15[1]) - 1) * 100, Number(page15[1]) * 100));
    if (url === '/repos/acme/widgets/pulls/16' && auth === 'token ghs_test') return reply(200, { number: 16, title: 't', body: 'Fixes #17, closes #3', user: { login: 'dev' }, head: { sha: 'abc1230000000000000000000000000000000000', ref: 'fix' }, base: { ref: 'main' } });
    if (url === '/repos/acme/widgets/pulls/18/reviews') return reply(422, { message: 'Unprocessable' });
    if (url === '/repos/acme/widgets/pulls/28') return reply(200, { number: 28, user: { login: 'dev' }, head: { sha: 'abc1230000000000000000000000000000000000' } });
    if (url === '/repos/acme/widgets/pulls/28/reviews') return reply(409, { message: 'Conflict' });
    if (url === '/repos/acme/widgets/issues/17') return reply(410, { message: 'This issue was deleted' });
    if (url === '/graphql' && graphqlErrors) return reply(200, { errors: [{ message: 'Resource not accessible by integration' }] });
    if (url === '/graphql') return reply(200, { data: { repository: { pullRequest: { reviewThreads: { nodes: threads } } } } });
    reply(404, {});
  }),
);

const invocations: Array<{ path: string; body: any }> = [];
// Every invocation body, refused ones too.
const attempts: any[] = [];
const watchers: string[] = [];
const watcherWaiters: Array<() => void> = [];
const holds: Record<string, Promise<void>> = {};
let busy = false;
let busyStatus = 409;
let busyDetail = 'session is mid-run as agent reviewer; implementer can start once it finishes';
const outcomes: Record<string, unknown> = {};
// The agent PR 7's session is mid-run as, by its /ping through the distributor: another agent's ping answers 409.
let runningAgent: string | undefined;
const distributor = await listen(
  createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    const ping = req.method === 'GET' && /^\/sessions\/github%3Aacme%2Fwidgets%237\/ping\?agent=(\w+)$/.exec(req.url!);
    if (ping && runningAgent && runningAgent !== ping[1]) return res.writeHead(409, { 'content-type': 'application/problem+json' }).end(JSON.stringify({ status: 'HealthyBusy', detail: `session runs agent ${runningAgent}, not ${ping[1]}` }));
    if (ping) return res.writeHead(200).end(JSON.stringify({ status: runningAgent ? 'HealthyBusy' : 'Healthy' }));
    if (req.url === '/agents') return res.writeHead(200).end(JSON.stringify({ agents: [{ name: 'reviewer', version: 1, status: 'promoted' }, { name: 'researcher', version: 1, status: 'draft' }] }));
    const body = JSON.parse(raw);
    if (body.wait) {
      watchers.push(body.requestId);
      watcherWaiters.splice(0).forEach((f) => f());
      await holds[body.requestId];
      return res.writeHead(200).end(JSON.stringify(outcomes[body.requestId] ?? {}));
    }
    attempts.push(body);
    if (busy) return res.writeHead(busyStatus, { 'content-type': 'application/problem+json' }).end(JSON.stringify({ detail: busyDetail }));
    invocations.push({ path: decodeURIComponent(req.url!), body });
    res.writeHead(202).end('{}');
  }),
);

const config = { webhookSecret: 'hook-secret', distributorUrl: distributor, label: 'quack:review', agent: 'reviewer' };
const service = await listen(serveApp(createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), config)));

function deliver(event: string, payload: unknown, opts: { secret?: string; id?: string; to?: string } = {}) {
  const body = JSON.stringify(payload);
  const sig = `sha256=${createHmac('sha256', opts.secret ?? 'hook-secret').update(body).digest('hex')}`;
  return fetch(`${opts.to ?? service}/webhook`, { method: 'POST', body, headers: { 'x-github-event': event, 'x-hub-signature-256': sig, 'x-github-delivery': opts.id ?? 'd-1' } }).then((r) => r.status);
}

const repository = { name: 'widgets', full_name: 'acme/widgets', owner: { login: 'acme' } };
const pr = { number: 7, title: 'Fix the cache', body: 'Closes #3', head: { sha: 'abc1230000000000000000000000000000000000', ref: 'fix' }, base: { ref: 'main' } };

test('verified webhooks start a review in the PR session, keyed by the delivery id', async () => {
  invocations.length = 0;
  assert.equal(await deliver('pull_request', { action: 'labeled', label: { name: 'quack:review' }, pull_request: pr, repository }, { secret: 'wrong' }), 401);
  assert.equal(await deliver('pull_request', { action: 'labeled', label: { name: 'bug' }, pull_request: pr, repository }), 200);
  assert.equal(await deliver('pull_request', { action: 'labeled', label: { name: 'quack:review' }, pull_request: pr, repository }, { id: 'd-7' }), 202);
  assert.equal(invocations.length, 1);
  assert.equal(invocations[0].path, '/sessions/github:acme/widgets#7/invocations?agent=reviewer');
  assert.equal(invocations[0].body.requestId, 'github-delivery:d-7');
  assert.match(invocations[0].body.input, /Review pull request acme\/widgets#7 \(label quack:review\)/);
  assert.match(invocations[0].body.input, /Head: abc1230000000000000000000000000000000000 on fix; base: main/);
  assert.equal(invocations[0].body.title, 'Fix the cache');
  // The opening card's data rides in meta, keyed by head sha; the model's input text carries none of it.
  assert.deepEqual(invocations[0].body.meta.files, [{ path: 'src/a.ts', additions: 5, deletions: 2 }]);
  assert.deepEqual(invocations[0].body.meta.linked, [{ number: 3, title: 'bug' }]);
  assert.equal(invocations[0].body.meta.id, 'abc1230000000000000000000000000000000000');
  assert.doesNotMatch(invocations[0].body.input, /src\/a.ts/);
});

test('a push to a labelled PR starts a review of the new head; an unlabelled PR is ignored', async () => {
  invocations.length = 0;
  const pushed = { ...pr, head: { sha: 'def456', ref: 'fix' }, labels: [{ name: 'quack:review' }] };
  assert.equal(await deliver('pull_request', { action: 'synchronize', pull_request: { ...pushed, labels: [{ name: 'bug' }] }, repository }, { id: 'd-sync0' }), 200);
  assert.equal(await deliver('pull_request', { action: 'synchronize', pull_request: pushed, repository }, { id: 'd-sync1' }), 202);
  assert.equal(invocations.length, 1);
  assert.equal(invocations[0].path, '/sessions/github:acme/widgets#7/invocations?agent=reviewer');
  assert.match(invocations[0].body.input, /Head: def456 on fix/);
});

test('no notice when a review was posted, and none for a run that is not final', () => {
  assert.equal(noticeFor({ status: 'unanswered', reason: 'run_failed' }, true), undefined);
  assert.equal(noticeFor({ status: 'done' }, true), undefined);
  assert.equal(noticeFor({}, false), undefined);
  assert.match(noticeFor({ status: 'done' }, false)!, /ended without a review being posted/);
});

test('/review starts a review only from a trusted commenter', async () => {
  invocations.length = 0;
  const comment = (association: string) => ({ action: 'created', issue: { ...pr, pull_request: {} }, comment: { body: ' /review ', author_association: association, user: { login: 'someone' } }, repository });
  assert.equal(await deliver('issue_comment', comment('CONTRIBUTOR')), 200);
  assert.equal(await deliver('issue_comment', comment('MEMBER'), { id: 'd-8' }), 202);
  assert.equal(invocations.length, 1);
  assert.match(invocations[0].body.input, /\/review by someone/);
});

test('/fix runs the implementer in the PR session only where one is configured; a deployment without it ignores /fix', async () => {
  invocations.length = 0;
  const fix = { action: 'created', issue: { ...pr, pull_request: {} }, comment: { body: '/fix the cache test\nand the lint error', author_association: 'MEMBER', user: { login: 'someone' } }, repository };
  assert.equal(await deliver('issue_comment', fix, { id: 'd-fix0' }), 200);
  const withFix = await listen(serveApp(createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), { ...config, implementerAgent: 'implementer' })));
  const body = JSON.stringify(fix);
  const sig = `sha256=${createHmac('sha256', 'hook-secret').update(body).digest('hex')}`;
  const status = await fetch(`${withFix}/webhook`, { method: 'POST', body, headers: { 'x-github-event': 'issue_comment', 'x-hub-signature-256': sig, 'x-github-delivery': 'd-fix1' } }).then((r) => r.status);
  assert.equal(status, 202);
  assert.equal(invocations.length, 1);
  assert.equal(invocations[0].path, '/sessions/github:acme/widgets#7/invocations?agent=implementer');
  assert.match(invocations[0].body.input, /Instruction: the cache test\nand the lint error/);
});

const fixComment = { action: 'created', issue: { ...pr, pull_request: {} }, comment: { body: '/fix the cache test', author_association: 'MEMBER', user: { login: 'someone' } }, repository };
// The retry timer never fires inside a test; each test runs a retry pass itself with sweep().
const queuedService = (extra: object = {}) => {
  const svc = createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), { ...config, implementerAgent: 'implementer', queuePollMs: 3_600_000, ...extra });
  return listen(serveApp(svc)).then((url) => ({ svc, url }));
};
const fixService = queuedService;
const postFix = (url: string, id: string) => deliver('issue_comment', fixComment, { id, to: url });
const MID_RUN = 'session is mid-run as agent reviewer; implementer can start once it finishes';
const metaReads = () => calls.filter((c) => c.path === '/repos/acme/widgets/pulls/7/files?per_page=100').length;

test('a /fix that finds the session mid-run is queued behind it, answered once, and started once the session is idle', async () => {
  invocations.length = 0;
  commented.length = 0;
  const { svc, url } = await fixService();
  try {
    busy = true;
    runningAgent = 'reviewer';
    assert.equal(await postFix(url, 'd-q1'), 202);
    assert.equal(await postFix(url, 'd-q1'), 202, 'a redelivery');
    assert.deepEqual(commented, [fixQueued]);
    assert.equal(invocations.length, 0);
    const before = metaReads();
    await svc.sweep();
    assert.equal(invocations.length, 0, 'still mid-run, still waiting');
    assert.equal(metaReads(), before, 'retries reuse the stored opening-card data');
    busy = false;
    runningAgent = undefined;
    await svc.sweep();
    await svc.sweep();
    assert.equal(invocations.length, 1, 'started once');
    assert.equal(invocations[0].body.requestId, 'github-delivery:d-q1');
    assert.equal(invocations[0].path, '/sessions/github:acme/widgets#7/invocations?agent=implementer');
    assert.match(invocations[0].body.input, /Instruction: the cache test/);
    assert.equal(invocations[0].body.meta.id, 'abc1230000000000000000000000000000000000');
  } finally {
    busy = false;
    runningAgent = undefined;
    svc.stop();
  }
});

test('queued work survives a restart: a new service reading the same store starts it', async () => {
  invocations.length = 0;
  commented.length = 0;
  const db = join(mkdtempSync(join(tmpdir(), 'fixq-')), 'github.db');
  const first = await fixService({ waiting: sqliteWaiting(db) });
  busy = true;
  assert.equal(await postFix(first.url, 'd-q2'), 202);
  first.svc.stop();
  busy = false;
  assert.equal(invocations.length, 0);
  const store = sqliteWaiting(db);
  const second = await fixService({ waiting: store });
  try {
    await second.svc.sweep();
    assert.equal(invocations.length, 1);
    assert.equal(invocations[0].body.requestId, 'github-delivery:d-q2');
    assert.deepEqual(await store.list(), [], 'started work leaves the store');
  } finally {
    second.svc.stop();
  }
});

test('a /fix refused for any reason other than a running session is answered, not queued', async () => {
  invocations.length = 0;
  commented.length = 0;
  const { svc, url } = await fixService();
  try {
    busy = true;
    busyDetail = 'agent implementer is not registered';
    assert.equal(await postFix(url, 'd-q3'), 200);
    assert.deepEqual(commented, [fixRefused('agent implementer is not registered')]);
    busy = false;
    await svc.sweep();
    assert.equal(invocations.length, 0, 'nothing was queued to start');
  } finally {
    busy = false;
    busyDetail = MID_RUN;
    svc.stop();
  }
});

// A run-end the way the distributor delivers it, and the wait for the retry it starts (it is not awaitable).
type RunEnd = (session: string, event: { type: string }) => void;
const runEnds = () => {
  const heard: RunEnd[] = [];
  const subscribe = ((on: RunEnd) => (heard.push(on), () => void heard.splice(heard.indexOf(on), 1))) as never;
  return { subscribe, end: (session = 'github:acme/widgets#7') => heard.forEach((on) => on(session, { type: 'run.ended' })) };
};
const until = async (done: () => boolean) => { for (let i = 0; i < 200 && !done(); i++) await new Promise((r) => setImmediate(r)); };

test('queued work starts when the session run ends, with no timer and no sweep', async () => {
  invocations.length = 0;
  const { subscribe, end } = runEnds();
  const { svc, url } = await queuedService({ subscribe });
  try {
    busy = true;
    assert.equal(await deliver('pull_request', labeled, { id: 'd-ev1', to: url }), 202);
    busy = false;
    end('github:other/repo#1');
    await until(() => invocations.length > 0);
    assert.equal(invocations.length, 0, 'another session ending retries nothing of this one');
    end();
    await until(() => invocations.length > 0);
    assert.deepEqual(invocations.map((i) => i.body.requestId), ['github-delivery:d-ev1']);
  } finally {
    busy = false;
    svc.stop();
  }
});

test('review pushes while the reviewer runs are one steer on the newest head when the run ends', async () => {
  invocations.length = 0;
  const { subscribe, end } = runEnds();
  const { svc, url } = await queuedService({ subscribe });
  try {
    runningAgent = 'reviewer';
    assert.equal(await deliver('pull_request', pushed('abc123'), { id: 'd-ev2', to: url }), 202);
    assert.equal(await deliver('pull_request', pushed('def456'), { id: 'd-ev3', to: url }), 202);
    end();
    await until(() => invocations.length > 0);
    await until(() => false);
    assert.equal(invocations.length, 1, 'one steer, not one per push');
    assert.deepEqual(invocations.map((i) => [i.body.requestId, i.body.whenBusy]), [['github-delivery:d-ev3/steer/reviewer', 'steer']]);
    assert.equal(invocations[0].body.input, steerInput({ owner: 'acme', repo: 'widgets', number: 7, pushedHead: 'def456' }));
  } finally {
    runningAgent = undefined;
    svc.stop();
  }
});

test('with no run-end event the slow sweep still retries queued work', async () => {
  invocations.length = 0;
  let sweep = () => {};
  let every = 0;
  const queueTimers = { setInterval: (fn: () => void, ms: number) => (sweep = fn, every = ms, {}), clearInterval: () => {} };
  const { svc, url } = await queuedService({ queueTimers, queuePollMs: undefined });
  try {
    busy = true;
    assert.equal(await deliver('pull_request', labeled, { id: 'd-ev4', to: url }), 202);
    busy = false;
    assert.ok(every >= 60_000, 'a slow sweep');
    sweep();
    await until(() => invocations.length > 0);
    assert.deepEqual(invocations.map((i) => i.body.requestId), ['github-delivery:d-ev4']);
  } finally {
    busy = false;
    svc.stop();
  }
});

const labeled = { action: 'labeled', label: { name: 'quack:review' }, pull_request: pr, repository };
const pushed = (sha: string) => ({ action: 'synchronize', pull_request: { ...pr, head: { sha, ref: 'fix' }, labels: [{ name: 'quack:review' }] }, repository });

test('a review that finds another agent in the session is queued, a newer push replaces it, and it starts once the session is free', async () => {
  invocations.length = 0;
  attempts.length = 0;
  commented.length = 0;
  const { svc, url } = await queuedService();
  try {
    busy = true;
    runningAgent = 'implementer';
    busyDetail = 'session is mid-run as agent implementer; reviewer can start once it finishes';
    assert.equal(await deliver('pull_request', labeled, { id: 'd-rq1', to: url }), 202, 'handled, so never redelivered');
    assert.equal(await deliver('pull_request', { ...pushed('def456'), sender: { login: 'quack-app[bot]' } }, { id: 'd-rq2', to: url }), 202);
    assert.deepEqual(commented, [reviewQueued], 'said once, not once per push');
    await svc.sweep();
    assert.equal(invocations.length, 0);
    assert.ok(attempts.every((a) => a.whenBusy === undefined), 'never steered into the implementer');
    busy = false;
    runningAgent = undefined;
    await svc.sweep();
    await svc.sweep();
    assert.deepEqual(invocations.map((i) => [i.body.requestId, i.body.whenBusy]), [['github-delivery:d-rq2', undefined]], 'only the newest head is reviewed, once, as a plain run');
    assert.equal(invocations[0].path, '/sessions/github:acme/widgets#7/invocations?agent=reviewer');
    assert.match(invocations[0].body.input, /Head: def456 on fix/);
  } finally {
    busy = false;
    runningAgent = undefined;
    busyDetail = MID_RUN;
    svc.stop();
  }
});

test('a push while the reviewer runs is sent as a steer; once that run ends without reviewing the head, a run starts for it', async () => {
  invocations.length = 0;
  const { svc, url } = await queuedService();
  try {
    runningAgent = 'reviewer';
    assert.equal(await deliver('pull_request', pushed('def456'), { id: 'd-st1', to: url }), 202);
    assert.equal(invocations.length, 0, 'no follow-up queued in the session');
    await svc.sweep();
    await svc.sweep();
    assert.deepEqual(invocations.map((i) => [i.body.requestId, i.body.whenBusy]), [['github-delivery:d-st1/steer/reviewer', 'steer'], ['github-delivery:d-st1/steer/reviewer', 'steer']], 'a resend is the same steer');
    assert.equal(invocations[0].body.input, steerInput({ owner: 'acme', repo: 'widgets', number: 7, pushedHead: 'def456' }));
    runningAgent = undefined;
    await svc.sweep();
    await svc.sweep();
    assert.equal(invocations.length, 3);
    assert.deepEqual([invocations[2].body.requestId, invocations[2].body.whenBusy], ['github-delivery:d-st1', undefined]);
    assert.match(invocations[2].body.input, /Review pull request acme\/widgets#7 .*\nTitle: Fix the cache\nHead: def456 on fix/);
  } finally {
    runningAgent = undefined;
    svc.stop();
  }
});

test('a steered run still going at the queue age limit is said on the PR and steered no more', async (t) => {
  invocations.length = 0;
  commented.length = 0;
  const store = sqliteWaiting(':memory:');
  const { svc, url } = await queuedService({ waiting: store });
  try {
    t.mock.timers.enable({ apis: ['Date'], now: Date.now() });
    runningAgent = 'reviewer';
    assert.equal(await deliver('pull_request', pushed('def456'), { id: 'd-st5', to: url }), 202);
    await svc.sweep();
    assert.equal(invocations.length, 1);
    t.mock.timers.tick(6 * 3600_000);
    await svc.sweep();
    await svc.sweep();
    assert.equal(invocations.length, 1, 'no steer after the limit');
    assert.deepEqual(commented, [reviewRefused('the steered run did not finish in 6 hours')]);
    assert.deepEqual(await store.list(), []);
  } finally {
    runningAgent = undefined;
    svc.stop();
  }
});

test('a push while the reviewer is idle starts a run at once, never a steer', async () => {
  invocations.length = 0;
  const { svc, url } = await queuedService();
  try {
    assert.equal(await deliver('pull_request', pushed('def456'), { id: 'd-st2', to: url }), 202);
    assert.deepEqual(invocations.map((i) => [i.body.requestId, i.body.whenBusy]), [['github-delivery:d-st2', undefined]]);
    assert.match(invocations[0].body.input, /Head: def456 on fix/);
  } finally {
    svc.stop();
  }
});

test('two quick pushes while the reviewer runs make one steer for the newest head; once a review covers it, nothing more starts', async () => {
  invocations.length = 0;
  const { svc, url } = await queuedService();
  let undo = () => {};
  try {
    runningAgent = 'reviewer';
    assert.equal(await deliver('pull_request', pushed('def456'), { id: 'd-st3', to: url }), 202);
    assert.equal(await deliver('pull_request', pushed('0ff1ce'), { id: 'd-st4', to: url }), 202);
    await svc.sweep();
    assert.deepEqual(invocations.map((i) => i.body.requestId), ['github-delivery:d-st4/steer/reviewer']);
    assert.match(invocations[0].body.input, /head is now 0ff1ce/);
    assert.doesNotMatch(invocations[0].body.input, /def456/);
    undo = reviewedAt('0ff1ce');
    runningAgent = undefined;
    await svc.sweep();
    assert.equal(invocations.length, 1, 'the steered run reviewed the head');
  } finally {
    undo();
    runningAgent = undefined;
    svc.stop();
  }
});

test('a /fix while the implementer runs is sent as a steer, each instruction its own; a plain comment steers nothing', async () => {
  invocations.length = 0;
  const { svc, url } = await fixService();
  try {
    runningAgent = 'implementer';
    commented.length = 0;
    assert.equal(await postFix(url, 'd-fs1'), 202);
    assert.equal(await postFix(url, 'd-fs1'), 202, 'a redelivery');
    assert.equal(await deliver('issue_comment', { ...fixComment, comment: { ...fixComment.comment, body: '/fix and the lint error' } }, { id: 'd-fs2', to: url }), 202);
    assert.equal(await deliver('issue_comment', { ...fixComment, comment: { ...fixComment.comment, body: 'looks close' } }, { id: 'd-fs3', to: url }), 200);
    assert.equal(invocations.length, 0, 'no follow-up queued in the session');
    await svc.sweep();
    await svc.sweep();
    assert.deepEqual(invocations.map((i) => [i.path, i.body.requestId, i.body.whenBusy]).sort(), [
      ['/sessions/github:acme/widgets#7/invocations?agent=implementer', 'github-delivery:d-fs1/steer/implementer', 'steer'],
      ['/sessions/github:acme/widgets#7/invocations?agent=implementer', 'github-delivery:d-fs2/steer/implementer', 'steer'],
    ], 'one steer each, not resent');
    assert.match(invocations.find((i) => i.body.requestId === 'github-delivery:d-fs1/steer/implementer')!.body.input, /^someone sent \/fix on pull request acme\/widgets#7\. Instruction: the cache test\nIf you are already changing it/);
    assert.deepEqual(commented, [fixSteered, fixSteered], 'each /fix is answered once, a redelivery not again');
  } finally {
    runningAgent = undefined;
    svc.stop();
  }
});

const branchPush = (sha: string, login: string) => ({ ...pushed(sha), pull_request: { ...pushed(sha).pull_request, labels: [] }, sender: { login } });

test("a person's pushes to the branch while the implementer runs make one steer for the newest head; its own push and an idle implementer make none", async () => {
  invocations.length = 0;
  const { svc, url } = await fixService();
  try {
    runningAgent = 'implementer';
    assert.equal(await deliver('pull_request', branchPush('def456', 'dev'), { id: 'd-bm1', to: url }), 202);
    assert.equal(await deliver('pull_request', branchPush('0ff1ce', 'dev'), { id: 'd-bm2', to: url }), 202);
    assert.equal(await deliver('pull_request', branchPush('b0b000', 'Quack-App[bot]'), { id: 'd-bm3', to: url }), 200, 'its own push');
    await svc.sweep();
    await svc.sweep();
    assert.deepEqual(invocations.map((i) => [i.path, i.body.requestId, i.body.whenBusy]), [['/sessions/github:acme/widgets#7/invocations?agent=implementer', 'github-delivery:d-bm2/steer/implementer', 'steer']]);
    assert.match(invocations[0].body.input, /^dev pushed to the branch of pull request acme\/widgets#7: it is now at 0ff1ce\. .*call checkout_task again/);
    runningAgent = undefined;
    assert.equal(await deliver('pull_request', branchPush('c0ffee', 'dev'), { id: 'd-bm4', to: url }), 200);
    await svc.sweep();
    assert.equal(invocations.length, 1);
  } finally {
    runningAgent = undefined;
    svc.stop();
  }
});

test("a person's push queued while the implementer ran starts nothing once it has stopped", async () => {
  invocations.length = 0;
  const store = sqliteWaiting(':memory:');
  const { svc, url } = await fixService({ waiting: store });
  try {
    runningAgent = 'implementer';
    assert.equal(await deliver('pull_request', branchPush('def456', 'dev'), { id: 'd-bm5', to: url }), 202);
    runningAgent = undefined;
    await svc.sweep();
    assert.equal(invocations.length, 0);
    assert.deepEqual(await store.list(), []);
  } finally {
    runningAgent = undefined;
    svc.stop();
  }
});

test("a GitHub fault while checking a push's pusher still starts the review the push asks for", async () => {
  invocations.length = 0;
  const down = { slug: async () => { throw new Error('GitHub 502'); }, asInstallation: async () => ({ status: 404, json: undefined, link: '' }) } as unknown as GithubApp;
  const url = await listen(serveApp(createGithubService(down, { ...config, implementerAgent: 'implementer', queuePollMs: 3_600_000 })));
  assert.equal(await deliver('pull_request', { ...pushed('def456'), sender: { login: 'dev' } }, { id: 'd-bm7', to: url }), 202);
  assert.deepEqual(invocations.map((i) => [i.path, i.body.requestId]), [['/sessions/github:acme/widgets#7/invocations?agent=reviewer', 'github-delivery:d-bm7']]);
});

test('a steer the session keeps refusing is given up on at the queue age limit', async (t) => {
  invocations.length = 0;
  const store = sqliteWaiting(':memory:');
  const { svc, url } = await fixService({ waiting: store });
  try {
    t.mock.timers.enable({ apis: ['Date'], now: Date.now() });
    runningAgent = 'implementer';
    assert.equal(await deliver('pull_request', branchPush('def456', 'dev'), { id: 'd-bm6', to: url }), 202);
    busy = true;
    busyStatus = 400;
    await svc.sweep();
    assert.equal((await store.list()).length, 1, 'refused, so it waits');
    t.mock.timers.tick(6 * 3600_000);
    await svc.sweep();
    assert.deepEqual(await store.list(), []);
    assert.equal(invocations.length, 0);
  } finally {
    busy = false;
    busyStatus = 409;
    runningAgent = undefined;
    svc.stop();
  }
});

// The bot's review of PR 7 at `sha`, listed until the returned undo runs.
function reviewedAt(sha: string) {
  const mine = review(50, 'quack-app[bot]', 'APPROVED', 'ok', sha);
  reviews.push(mine);
  return () => void reviews.splice(reviews.indexOf(mine), 1);
}

async function busyThen(url: string, svc: { sweep(): Promise<void> }, id: string, event: object, between: () => void, name = 'pull_request') {
  busy = true;
  busyDetail = MID_RUN;
  try {
    assert.equal(await deliver(name, event, { id, to: url }), 202);
    between();
  } finally {
    busy = false;
  }
  await svc.sweep();
}

test('a push queued mid-review starts no second run once a review is posted on that head', async () => {
  invocations.length = 0;
  const { svc, url } = await queuedService();
  let undo = () => {};
  try {
    await busyThen(url, svc, 'd-dup1', pushed('def456'), () => { undo = reviewedAt('def456'); });
    assert.equal(invocations.length, 0);
  } finally {
    undo();
    svc.stop();
  }
});

test('a label re-trigger starts a run on a reviewed head, since a person asked; a push for that head does not, a new head does', async () => {
  invocations.length = 0;
  const { svc, url } = await queuedService();
  const undo = reviewedAt('abc1230000000000000000000000000000000000');
  try {
    assert.equal(await deliver('pull_request', labeled, { id: 'd-dup2', to: url }), 202);
    assert.equal(await deliver('pull_request', pushed('abc1230000000000000000000000000000000000'), { id: 'd-dup5', to: url }), 200);
    assert.deepEqual(invocations.map((i) => i.body.requestId), ['github-delivery:d-dup2']);
    assert.equal(await deliver('pull_request', pushed('def456'), { id: 'd-dup3', to: url }), 202);
    assert.deepEqual(invocations.map((i) => i.body.requestId), ['github-delivery:d-dup2', 'github-delivery:d-dup3']);
  } finally {
    undo();
    svc.stop();
  }
});

test('a label, a push and a /review queued for a PR that closed or merged before they start are all dropped', async () => {
  const reviewCmd = { action: 'created', issue: { ...pr, pull_request: {} }, comment: { body: '/review', author_association: 'MEMBER', user: { login: 'someone' } }, repository };
  const { svc, url } = await queuedService();
  try {
    const kinds = [['pull_request', labeled], ['pull_request', pushed('def456')], ['issue_comment', reviewCmd]] as const;
    for (const [i, [event, payload]] of kinds.entries()) {
      invocations.length = 0;
      commented.length = 0;
      await busyThen(url, svc, `d-dup4-${i}`, payload, () => { pr7State = 'closed'; pr7Merged = true; }, event);
      await svc.sweep();
      assert.equal(invocations.length, 0, event + i);
      // A person who asked is told once; an automatic push is dropped silently.
      assert.deepEqual(commented.filter((c) => !c.includes('queued')), i === 1 ? [] : ['No review started: this pull request is merged.']);
      pr7State = 'open';
      pr7Merged = false;
    }
  } finally {
    pr7State = 'open';
    pr7Merged = false;
    svc.stop();
  }
});

test('a review queued behind a /fix waits beside it; neither replaces the other', async () => {
  invocations.length = 0;
  commented.length = 0;
  const { svc, url } = await queuedService();
  try {
    busy = true;
    assert.equal(await postFix(url, 'd-both1'), 202);
    assert.equal(await deliver('pull_request', labeled, { id: 'd-both2', to: url }), 202);
    busy = false;
    await svc.sweep();
    assert.deepEqual(invocations.map((i) => i.body.requestId).sort(), ['github-delivery:d-both1', 'github-delivery:d-both2']);
  } finally {
    busy = false;
    svc.stop();
  }
});

test('a review refused for any other reason is said on the PR, not dropped', async () => {
  invocations.length = 0;
  commented.length = 0;
  const { svc, url } = await queuedService();
  try {
    busy = true;
    busyDetail = 'agent reviewer version 2 changed since it was first used; bump its version';
    assert.equal(await deliver('pull_request', labeled, { id: 'd-rr1', to: url }), 200);
    assert.deepEqual(commented, [reviewRefused('agent reviewer version 2 changed since it was first used; bump its version')]);
    busy = false;
    await svc.sweep();
    assert.equal(invocations.length, 0);
  } finally {
    busy = false;
    busyDetail = MID_RUN;
    svc.stop();
  }
});

test('a distributor outage queues the start instead of answering 502 for a redelivery that may never come', async () => {
  invocations.length = 0;
  commented.length = 0;
  const { svc, url } = await queuedService();
  try {
    busy = true;
    busyStatus = 503;
    assert.equal(await deliver('pull_request', labeled, { id: 'd-out1', to: url }), 202);
    assert.deepEqual(commented, [], 'an outage is not worth a comment');
    busy = false;
    await svc.sweep();
    assert.deepEqual(invocations.map((i) => i.body.requestId), ['github-delivery:d-out1']);
  } finally {
    busy = false;
    busyStatus = 409;
    svc.stop();
  }
});

test('/implement on an issue runs the implementer in the issue session; a refused start is answered on the issue', async () => {
  invocations.length = 0;
  const implement = { action: 'created', issue: { number: 12, title: 'Add TTL', body: 'b', labels: [{ name: 'bug' }] }, comment: { body: '/implement', author_association: 'MEMBER', user: { login: 'someone' } }, repository };
  const withFix = await listen(serveApp(createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), { ...config, implementerAgent: 'implementer' })));
  const body = JSON.stringify(implement);
  const sig = `sha256=${createHmac('sha256', 'hook-secret').update(body).digest('hex')}`;
  const send = (id: string) => fetch(`${withFix}/webhook`, { method: 'POST', body, headers: { 'x-github-event': 'issue_comment', 'x-hub-signature-256': sig, 'x-github-delivery': id } }).then((r) => r.status);
  assert.equal(await send('d-imp1'), 202);
  assert.equal(invocations[0].path, '/sessions/github:acme/widgets#12/invocations?agent=implementer');
  assert.match(invocations[0].body.input, /Implement issue acme\/widgets#12/);

  busy = true;
  commented.length = 0;
  assert.equal(await send('d-imp2'), 200);
  busy = false;
  assert.equal(invocations.length, 1);
  assert.deepEqual(commented, [fixRefused('session is mid-run as agent reviewer; implementer can start once it finishes', true)]);
  assert.match(commented[0], /`\/implement` on this issue/);
});

test('the issue route returns an issue with the default branch; a pull request number is not an issue', async () => {
  const get = (n: number, token = 'svc-secret') => fetch(`${service}/issues/acme/widgets/${n}`, { headers: { authorization: `Bearer ${token}` } }).then(async (r) => ({ status: r.status, body: await r.json() }));
  assert.deepEqual((await get(12)).body, { owner: 'acme', repo: 'widgets', number: 12, title: 'Add TTL', body: 'b', author: 'dev', state: 'open', labels: ['bug'], baseRef: 'trunk' });
  assert.equal((await get(9)).status, 404);
  const onV1 = await listen(serveApp(createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), { ...config, issueBase: { 'acme/widgets': 'v1', 'acme/other': 'next' } })));
  const issue = await fetch(`${onV1}/issues/acme/widgets/12`).then((r) => r.json());
  assert.equal(issue.baseRef, 'v1', 'the listed repository uses its issueBase');
  const casedOnly = await listen(serveApp(createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), { ...config, issueBase: { 'Acme/Widgets': 'v1' } })));
  assert.equal((await fetch(`${casedOnly}/issues/acme/widgets/12`).then((r) => r.json())).baseRef, 'v1', 'the lookup ignores case, as prKey does');
  const elsewhere = await listen(serveApp(createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), { ...config, issueBase: { 'acme/other': 'next' } })));
  assert.equal((await fetch(`${elsewhere}/issues/acme/widgets/12`).then((r) => r.json())).baseRef, 'trunk', 'an unlisted repository keeps its default branch');
});

// POST /pulls/{owner}/{repo}/{number}/reviews; the target leaves the body, and each post gets its own requestId unless it names one.
let requests = 0;
const mark = () => `\n\n<!-- quack-delivery: rq-${requests} -->`;
const reviewAt = (target: string, body: unknown) => fetch(`${service}/pulls/${target}/reviews`, { method: 'POST', body: JSON.stringify(body), headers: { authorization: 'Bearer svc-secret' } });
const postTo = ({ target = 'acme/widgets/7', ...body }: Record<string, unknown>) =>
  reviewAt(target as string, { requestId: `rq-${++requests}`, ...body }).then(async (r) => ({ status: r.status, body: await r.json() }));
const problemShape = (b: any, status: number) => assert.deepEqual([b.type, b.status, b.title, typeof b.detail, b.error], ['about:blank', status, STATUS_CODES[status], 'string', b.detail]);

test('a pull request is opened with the installation token, mentions of the bot defanged', async () => {
  const open = (body: unknown) => fetch(`${service}/pulls/acme/widgets`, { method: 'POST', body: JSON.stringify(body), headers: { authorization: 'Bearer svc-secret' } }).then(async (r) => ({ status: r.status, body: await r.json() }));
  assert.equal((await open({ head: 'quack/12-x', base: 'main', title: 't' })).status, 400);
  calls.length = 0;
  const made = await open({ head: 'quack/12-x', base: 'trunk', title: 'feat: x', body: 'ping @quack-app; keep `@quack-app`, @quack-apple, me@quack-app.dev\n\nCloses #12' });
  assert.deepEqual(made, { status: 201, body: { number: 13, url: 'https://github.com/acme/widgets/pull/13' } });
  const sent = calls.find((c) => c.path === '/repos/acme/widgets/pulls')!;
  assert.equal(sent.auth, 'token ghs_test');
  assert.deepEqual(sent.body, { head: 'quack/12-x', base: 'trunk', title: 'feat: x', body: 'ping quack-app; keep `@quack-app`, @quack-apple, me@quack-app.dev\n\nCloses #12' });
});

const credential = (url: string, token = 'svc-secret') =>
  fetch(`${service}/git-credential?url=${encodeURIComponent(url)}`, { headers: { authorization: `Bearer ${token}` } }).then(async (r) => ({ status: r.status, headers: r.headers, body: await r.json() }));
const mints = () => calls.filter((c) => c.path.endsWith('/access_tokens'));

test('a git credential is minted for the named repository with contents: write only, and cached until near expiry', async () => {
  calls.length = 0;
  const first = await credential('https://github.com/acme/widgets.git');
  assert.equal(first.status, 200);
  assert.equal(first.headers.get('cache-control'), 'no-store');
  assert.deepEqual([first.body.username, first.body.token], ['x-access-token', 'ghs_scoped_widgets']);
  assert.match(first.body.expiresAt, /^\d{4}-\d\d-\d\dT.*Z$/);
  assert.deepEqual(Object.keys(first.body).sort(), ['expiresAt', 'token', 'username']);
  assert.deepEqual(mints().map((c) => c.body), [{ repositories: ['widgets'], permissions: { contents: 'write' } }]);
  assert.equal((await credential('https://github.com/acme/widgets')).body.expiresAt, first.body.expiresAt);
  assert.equal(mints().length, 1, 'the second request uses the cached token');
  assert.equal((await credential('https://github.com/ACME/widgets')).body.token, 'ghs_scoped_widgets', 'the owner cases the same repository');
  assert.equal(mints().length, 1);
  assert.equal((await credential('https://github.com/acme/private.git')).status, 404, 'the app is not installed there');
});

test('a scoped token near expiry is minted again, by the same skew rule as the service token', async () => {
  scopedLifeMs = 30_000;
  try {
    calls.length = 0;
    await credential('https://github.com/acme/other');
    await credential('https://github.com/acme/other');
    assert.equal(mints().length, 2, 'under a minute left is not served from the cache');
  } finally {
    scopedLifeMs = 3600_000;
  }
});

test('a scoped token is never served for another repository, nor used for the service itself', async () => {
  calls.length = 0;
  const widgets = await credential('https://github.com/acme/widgets');
  const other = await credential('https://github.com/acme/other.git');
  assert.deepEqual([widgets.body.token, other.body.token], ['ghs_scoped_widgets', 'ghs_scoped_other']);
  assert.deepEqual(mints().map((c) => c.body.repositories), [['other']], 'widgets was cached; other minted for itself');
  assert.equal((await credential('https://github.com/acme/widgets')).body.token, 'ghs_scoped_widgets', 'and widgets is still its own');
});

test("the service's own calls keep the full installation token, cached apart from a session's scoped one", async () => {
  const fresh = await listen(serveApp(createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), config)));
  calls.length = 0;
  const scoped = await fetch(`${fresh}/git-credential?url=${encodeURIComponent('https://github.com/acme/widgets')}`).then((r) => r.json());
  assert.equal(scoped.token, 'ghs_scoped_widgets');
  assert.equal((await fetch(`${fresh}/pulls/acme/widgets/7/head`)).status, 200);
  assert.deepEqual(mints().map((c) => c.body), [{ repositories: ['widgets'], permissions: { contents: 'write' } }, undefined], 'the service minted its own token with no repositories or permissions');
  assert.ok(calls.filter((c) => c.path.startsWith('/repos/acme/widgets/pulls/7')).every((c) => c.auth === 'token ghs_test'));
  await fetch(`${fresh}/pulls/acme/widgets/7/head`);
  assert.equal(mints().length, 2, 'each cache serves its own');
});

test('a git credential url that is not exactly a github.com repository mints nothing', async () => {
  calls.length = 0;
  for (const url of [
    'https://gitlab.com/acme/widgets.git', 'https://github.com.evil.example/acme/widgets', 'https://evil.example/github.com/acme/widgets', 'https://github.com@evil.example/acme/widgets',
    'https://notgithub.com/acme/widgets', 'https://github.com/acme/widgets/extra', 'https://github.com/acme/widgets/tree/main', 'https://github.com/acme', 'https://github.com/', 'https://github.com/acme/.git', 'not a url', '',
  ]) {
    const r = await credential(url);
    assert.equal(r.status, 404, url);
    problemShape(r.body, 404);
  }
  assert.deepEqual(calls, [], 'nothing reached GitHub');
  assert.equal((await credential('https://github.com/acme/widgets/')).status, 200, 'a trailing slash is still the repository');
});

test('a git credential refusal is a problem document', async () => {
  assert.equal((await credential('https://github.com/acme/private')).headers.get('content-type'), 'application/problem+json');
});

test('a sealed review is posted with the installation token, never handed to the session', async () => {
  assert.equal((await postTo({ event: 'MERGE', body: 'x' })).status, 400);
  const posted = await postTo({ commitId: 'abc1230000000000000000000000000000000000', event: 'REQUEST_CHANGES', body: '## Verdict\nrequest_changes' });
  assert.equal(posted.status, 201);
  assert.equal(posted.body.url, 'https://github.com/acme/widgets/pull/7#review-9');
  const sent = calls.findLast((c) => c.path.endsWith('/reviews'))!;
  assert.match(sent.body.body, /^## Verdict\nrequest_changes\n\n<!-- quack-delivery: rq-\d+ -->$/);
  assert.deepEqual({ ...sent.body, body: undefined }, { event: 'REQUEST_CHANGES', body: undefined, commit_id: 'abc1230000000000000000000000000000000000' });
});

test('posting a review minimizes the app\u2019s earlier ones with a node id; a person\u2019s and the new review are left alone', async () => {
  const mine = review(51, 'quack-app[bot]', 'COMMENTED', 'earlier', 'old111', 'node-bot-1');
  const theirs = review(52, 'maria', 'COMMENTED', 'still open', 'old111', 'node-human-1');
  reviews.push(mine, theirs);
  calls.length = 0;
  try {
    assert.equal((await postTo({ commitId: 'abc1230000000000000000000000000000000000', event: 'APPROVE', body: '## Verdict\napprove' })).status, 201);
    const minimized = calls.filter((c) => c.path === '/graphql');
    assert.equal(minimized.length, 1, 'the bot\u2019s other review has no node id; maria\u2019s is not the app\u2019s');
    assert.match(minimized[0].body.query, /minimizeComment[\s\S]*OUTDATED/);
    assert.deepEqual(minimized[0].body.variables, { id: 'node-bot-1' });
    assert.equal(minimized[0].auth, 'token ghs_test');
    assert.equal(minimized[0].version, undefined, 'GraphQL gets no REST version header');
  } finally {
    reviews.splice(reviews.indexOf(mine), 1);
    reviews.splice(reviews.indexOf(theirs), 1);
  }
});

test('a GraphQL failure minimizing old reviews is warned, never fails the post', async () => {
  const mine = review(53, 'quack-app[bot]', 'COMMENTED', 'earlier', 'old111', 'node-bot-2');
  reviews.push(mine);
  graphqlErrors = true;
  try {
    const posted = await postTo({ commitId: 'abc1230000000000000000000000000000000000', event: 'APPROVE', body: '## Verdict\napprove' });
    assert.equal(posted.status, 201);
    assert.deepEqual(posted.body, { id: 9, url: 'https://github.com/acme/widgets/pull/7#review-9' });
  } finally {
    graphqlErrors = false;
    reviews.splice(reviews.indexOf(mine), 1);
  }
});

test("an approve on the app's own pull request posts as a comment that leads with the verdict", async () => {
  const res = await postTo({ target: 'acme/widgets/14', commitId: 'abc1230000000000000000000000000000000000', event: 'APPROVE', body: '## Verdict\napprove' });
  assert.equal(res.status, 201);
  const sent = calls.findLast((c) => c.path.endsWith('/reviews'))!;
  assert.equal(sent.body.event, 'COMMENT');
  assert.match(sent.body.body, /^\*\*Verdict: approve\.\*\* .*\n\n## Verdict\napprove\n\n<!-- quack-delivery: rq-\d+ -->$/);
});

test("a PR edit sets the title and body with the installation token, mentions stripped", async () => {
  const edit = (body: unknown) => fetch(`${service}/pulls/acme/widgets/7`, { method: 'PATCH', body: JSON.stringify(body), headers: { authorization: 'Bearer svc-secret' } }).then((r) => r.status);
  assert.equal(await edit({ body: 'no title' }), 400);
  assert.equal(await edit({ title: 'fix: x', body: 'ping @quack-app\n\nFixes #3' }), 200);
  const sent = calls.findLast((c) => c.method === 'PATCH')!;
  assert.deepEqual([sent.path, sent.auth, sent.body], ['/repos/acme/widgets/pulls/7', 'token ghs_test', { title: 'fix: x', body: 'ping quack-app\n\nFixes #3' }]);
  const gone = await fetch(`${service}/pulls/acme/widgets/404`, { method: 'PATCH', body: JSON.stringify({ title: 't', body: 'b' }), headers: { authorization: 'Bearer svc-secret' } });
  assert.notEqual(gone.status, 200, 'a vanished pull request is not reported as edited');
});

test('owner and repo parse only from github.com URLs', () => {
  assert.deepEqual(ownerRepo('https://github.com/acme/widgets.git'), { owner: 'acme', repo: 'widgets' });
  assert.equal(ownerRepo('https://evil.com/acme/widgets'), undefined);
  assert.equal(ownerRepo('not a url'), undefined);
});

test('a pull request record carries every check run, past the first page', async () => {
  const res = await fetch(`${service}/pulls/acme/widgets/7`, { headers: { authorization: 'Bearer svc-secret' } });
  const pr = await res.json();
  assert.equal(pr.checks.length, 150);
  assert.deepEqual(pr.checks.filter((c: any) => c.conclusion === 'failure').map((c: any) => c.name), ['c2-49']);
  assert.deepEqual(pr.linkedIssues.map((i: any) => i.number), [3]);
});

test('a pull request record carries the open review feedback: latest human requests, unresolved threads only', async () => {
  const pr = await (await fetch(`${service}/pulls/acme/widgets/7`, { headers: { authorization: 'Bearer svc-secret' } })).json();
  assert.deepEqual(pr.feedback.reviews.map((r: any) => [r.author, r.state, r.current]), [['maria', 'CHANGES_REQUESTED', false], ['lee', 'COMMENTED', true]]);
  assert.deepEqual(pr.feedback.comments, [{ author: 'maria', path: 'app.ts', line: 4, body: 'Off by one.' }]);
  assert.equal(pr.feedback.commentsError, undefined);
  assert.equal(calls.findLast((c) => c.path === '/graphql')!.version, undefined, 'GraphQL gets no REST version header');
  assert.equal(calls.findLast((c) => c.path.includes('/reviews?'))!.version, '2022-11-28');
});

test("a pull request record carries the app's own inline comments with their replies, read across pages", async () => {
  const pr = await (await fetch(`${service}/pulls/acme/widgets/7`, { headers: { authorization: 'Bearer svc-secret' } })).json();
  assert.deepEqual(pr.feedback.ownThreads, [{ id: 800, path: 'app.ts', line: 4, commit: 'old999', at: '2026-11-09T22:58:00Z', outdated: false, body: 'question: exposed?', replies: [{ author: 'dev', at: '2026-11-09T23:01:00Z', body: 'Checked on prod.' }], repliesCut: 0 }]);
  assert.equal(pr.feedback.ownThreadsCut, 0);
});

test("a failed read of the app's own comments leaves the record readable, with the failure noted", async () => {
  reviewCommentsDown = true;
  try {
    const res = await fetch(`${service}/pulls/acme/widgets/7`, { headers: { authorization: 'Bearer svc-secret' } });
    assert.equal(res.status, 200);
    const pr = await res.json();
    assert.equal(pr.headSha, 'abc1230000000000000000000000000000000000');
    assert.equal(pr.feedback.ownThreads, undefined);
    assert.match(pr.feedback.ownThreadsError, /comments\?sort=created.*: 503/);
  } finally {
    reviewCommentsDown = false;
  }
});

test('GraphQL errors in a 200 answer are reported, not read as no threads', async () => {
  graphqlErrors = true;
  try {
    const pr = await (await fetch(`${service}/pulls/acme/widgets/7`, { headers: { authorization: 'Bearer svc-secret' } })).json();
    assert.deepEqual(pr.feedback.comments, []);
    assert.equal(pr.feedback.commentsError, 'Resource not accessible by integration');
  } finally {
    graphqlErrors = false;
  }
});

test('a commit with more check runs than the service reads is refused, not reviewed against part of its CI', async () => {
  const res = await fetch(`${service}/pulls/acme/widgets/8`, { headers: { authorization: 'Bearer svc-secret' } });
  assert.equal(res.status, 500, 'a limit here, not a GitHub outage to retry');
  assert.match((await res.json()).error, /more than 1000 check runs on full111/);
});

test('only a promoted agent can back the unattended trigger', async () => {
  await assertPromoted(distributor, 'reviewer');
  await assert.rejects(assertPromoted(distributor, 'researcher'), /researcher is draft/);
  await assert.rejects(assertPromoted(distributor, 'ghost'), /not registered/);
});

test('an unreachable distributor is retried with capped backoff, then the promotion check applies', async () => {
  const delays: number[] = [];
  const stop = new Error('stop retrying');
  const sleep = async (ms: number) => { delays.push(ms); if (delays.length === 7) throw stop; };
  await assert.rejects(assertPromoted('http://127.0.0.1:1', 'reviewer', sleep), stop);
  assert.deepEqual(delays, [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000]);

  let up = false;
  const url = await listen(createServer((_req, res) => { if (up) res.end(JSON.stringify({ agents: [{ name: 'reviewer', status: 'draft' }] })); else res.writeHead(503).end(); }));
  await assert.rejects(assertPromoted(url, 'reviewer', async () => { up = true; }), /reviewer is draft/);
});

test('a run that fails says so on the PR once, even when its delivery arrives twice', async () => {
  commented.length = 0;
  watchers.length = 0;
  outcomes['github-delivery:d-fail'] = { status: 'unanswered', reason: 'run_failed', detail: 'model refused the call' };
  const labeled = { action: 'labeled', label: { name: 'quack:review' }, pull_request: pr, repository };
  const noted = new Promise<void>((r) => commentWaiters.push(r));
  assert.equal(await deliver('pull_request', labeled, { id: 'd-fail' }), 202);
  assert.equal(await deliver('pull_request', labeled, { id: 'd-fail' }), 202);
  await noted;
  assert.deepEqual(commented, ['The review could not be completed (run_failed: model refused the call). Comment `/review` to try again.']);
  assert.equal(watchers.filter((w) => w === 'github-delivery:d-fail').length, 1);
});

// Holds a run's wait, posts a review for the PR as the session would, releases it, then runs a failing sentinel:
// its notice is the only comment if the delivered run was suppressed.
async function deliveredRunIsSilent(webhookOwner: string, id: string) {
  commented.length = 0;
  let release!: () => void;
  holds[`github-delivery:${id}`] = new Promise<void>((r) => (release = r));
  outcomes[`github-delivery:${id}`] = { status: 'done' };
  const started = new Promise<void>((r) => watcherWaiters.push(r));
  const labeled = { action: 'labeled', label: { name: 'quack:review' }, pull_request: pr, repository: { ...repository, owner: { login: webhookOwner } } };
  assert.equal(await deliver('pull_request', labeled, { id }), 202);
  await started;
  assert.equal((await postTo({ commitId: 'abc1230000000000000000000000000000000000', event: 'APPROVE', body: '## Verdict\napprove' })).status, 201);
  release();
  outcomes[`github-delivery:${id}-sentinel`] = { status: 'unanswered', reason: 'run_failed' };
  const noted = new Promise<void>((r) => commentWaiters.push(r));
  assert.equal(await deliver('pull_request', { ...labeled, repository }, { id: `${id}-sentinel` }), 202);
  await noted;
  assert.deepEqual(commented, ['The review could not be completed (run_failed). Comment `/review` to try again.']);
}

test('a run that delivered a review gets no notice', () => deliveredRunIsSilent('acme', 'd-delivered'));

test('a review counts for the run however the webhook cases the owner', () => deliveredRunIsSilent('ACME', 'd-delivered-case'));

test('a review posts its inline comments on the right side, mentions stripped; a 422 refusal posts the review as written, an outage is not retried', async () => {
  const post = (comments: unknown[]) => postTo({ commitId: 'abc1230000000000000000000000000000000000', event: 'COMMENT', body: 'cc @quack-app', comments }).then((r) => r.status);
  assert.equal(await post([{ path: 'src/a.ts', line: 3, body: '**nit**: ping @quack-app' }]), 201);
  assert.deepEqual(calls.findLast((c) => c.path.endsWith('/reviews'))!.body, { event: 'COMMENT', body: `cc quack-app${mark()}`, commit_id: 'abc1230000000000000000000000000000000000', comments: [{ path: 'src/a.ts', line: 3, side: 'RIGHT', body: '**nit**: ping quack-app' }] });
  const refused = [{ path: 'gone.ts', line: 3, body: '**nit**: lost?' }];
  const posts = () => calls.filter((c) => c.path.endsWith('/reviews')).length;
  const before = posts();
  const fallback = await postTo({ event: 'COMMENT', body: 'listed inline', fallbackBody: 'as written, @quack-app', comments: refused });
  assert.equal(fallback.status, 201);
  assert.equal(posts() - before, 2);
  assert.deepEqual(calls.findLast((c) => c.path.endsWith('/reviews'))!.body, { event: 'COMMENT', body: `as written, quack-app${mark()}` });
  // An outage is not a refusal: one attempt, a 502 to the session, nothing marked posted.
  const down = await postTo({ event: 'COMMENT', body: 'x', fallbackBody: 'y', comments: [{ path: 'down.ts', line: 1, body: 'n' }] });
  assert.equal(down.status, 502);
  assert.equal(posts() - before, 3);
});

test('a review delivery whose answer was lost is found on retry, not posted twice', async () => {
  const review = { target: 'acme/widgets/15', commitId: 'abc1230000000000000000000000000000000000', event: 'REQUEST_CHANGES', body: '## Verdict\nrequest_changes', requestId: 'conv-1/v1' };
  const first = await postTo(review);
  assert.equal(first.status, 201);
  assert.match(posted15[0].body, /\n\n<!-- quack-delivery: conv-1%2Fv1 -->$/);
  const again = await postTo(review);
  assert.deepEqual([again.status, again.body.url], [200, first.body.url]);
  assert.equal(posted15.length, 1, 'GitHub got one review');
  assert.equal((await postTo({ ...review, requestId: 'conv-1/v2' })).status, 201, 'another request posts');
  assert.equal(posted15.length, 2);
});

test('a deleted linked issue is absent from the record, not a failed read', async () => {
  const res = await fetch(`${service}/pulls/acme/widgets/16`, { headers: { authorization: 'Bearer svc-secret' } });
  assert.equal(res.status, 200);
  assert.deepEqual((await res.json()).linkedIssues.map((i: any) => i.number), [3]);
});

test('the head route reads the pull request once, without its checks, reviews or issues', async () => {
  await fetch(`${service}/pulls/acme/widgets/7/head`, { headers: { authorization: 'Bearer svc-secret' } });
  const before = calls.length;
  const res = await fetch(`${service}/pulls/acme/widgets/7/head`, { headers: { authorization: 'Bearer svc-secret' } });
  assert.deepEqual(await res.json(), { headSha: 'abc1230000000000000000000000000000000000', body: 'Fixes #3' });
  assert.deepEqual(calls.slice(before).map((c) => c.path), ['/repos/acme/widgets/pulls/7']);
  assert.equal((await fetch(`${service}/pulls/acme/widgets/404/head`)).status, 404);
});

test('a GitHub refusal keeps its status and a fault here is a 500, so neither is redelivered as an outage', async () => {
  assert.equal((await postTo({ target: 'acme/widgets/18', event: 'COMMENT', body: 'x' })).status, 422);
});

const mirror = (path: string) => fetch(`${service}/repos/${path}`).then(async (r) => ({ status: r.status, type: r.headers.get('content-type'), body: (await r.json()) as any }));
const reads = (n: number) => calls.filter((c) => c.path === `/repos/acme/widgets/pulls/${n}`).length;

// The first event off a stream, skipping keepalive comments.
async function nextEvent(reader: ReadableStreamDefaultReader<Uint8Array>) {
  let text = '';
  while (!/^event: .*\ndata: .*\n\n/m.test(text)) text += new TextDecoder().decode((await reader.read()).value);
  const [, name, data] = /^event: (.*)\ndata: (.*)$/m.exec(text)!;
  return { name, data: JSON.parse(data) };
}

test('closed and reopened webhooks write the mirrored PR, publish it as a pull_request event, and start no run', async () => {
  invocations.length = 0;
  const stream = await fetch(`${service}/events`);
  const reader = stream.body!.getReader();
  const closed = (n: number, merged: boolean) => ({ action: 'closed', pull_request: { ...pr, number: n, state: 'closed', merged, merged_at: merged ? '2026-11-03T10:00:00Z' : null, closed_at: '2026-11-03T10:00:00Z', html_url: `https://github.com/acme/widgets/pull/${n}`, labels: [{ name: 'quack:review' }] }, repository });
  assert.equal(await deliver('pull_request', closed(20, true), { id: 'd-c1' }), 200);
  assert.deepEqual(await nextEvent(reader), { name: 'pull_request', data: { action: 'closed', repository: { full_name: 'acme/widgets' }, pull_request: { number: 20, state: 'closed', merged: true, merged_at: '2026-11-03T10:00:00Z', closed_at: '2026-11-03T10:00:00Z', html_url: 'https://github.com/acme/widgets/pull/20', title: 'Fix the cache' } } });
  await reader.cancel();
  assert.equal(await deliver('pull_request', closed(21, false), { id: 'd-c2' }), 200);
  const reopened = (n: number) => ({ action: 'reopened', pull_request: { ...pr, number: n, state: 'open', merged: false, html_url: `https://github.com/acme/widgets/pull/${n}` }, repository });
  assert.equal(await deliver('pull_request', reopened(22), { id: 'd-c3' }), 200);
  assert.equal(invocations.length, 0);
  for (const [i, p] of [closed(23, false), reopened(23)].entries()) await deliver('pull_request', p, { id: `d-r${i}` });
  const before = calls.length;
  const got = await Promise.all(['acme/widgets/pulls/20', 'ACME/Widgets/pulls/21', 'acme/widgets/pulls/22', 'acme/WIDGETS/pulls/23'].map(mirror));
  assert.equal(calls.length, before, 'webhook writes need no GitHub read');
  assert.deepEqual(got.map((g) => [g.body.state, g.body.merged]), [['closed', true], ['closed', false], ['open', false], ['open', false]]);
  assert.equal(got[1].body.html_url, 'https://github.com/acme/widgets/pull/21', "GitHub's casing, not the request's");
});

test('the mirror reads an unseen PR from GitHub once, in GitHub\'s field names; merged and closed stay cached', async () => {
  const first = await Promise.all(['acme/widgets/pulls/9', 'acme/widgets/pulls/10', 'acme/widgets/pulls/11'].map(mirror));
  assert.deepEqual(first[0], { status: 200, type: 'application/json', body: { number: 9, state: 'closed', merged: true, merged_at: '2026-11-01T00:00:00Z', closed_at: '2026-11-01T00:00:00Z', html_url: 'https://github.com/acme/widgets/pull/9', title: 'Fix the cache' } });
  assert.deepEqual(first.map((f) => f.body.state), ['closed', 'closed', 'open']);
  await Promise.all(['acme/widgets/pulls/9', 'Acme/widgets/pulls/10', 'acme/widgets/pulls/11'].map(mirror));
  assert.deepEqual([reads(9), reads(10), reads(11)], [1, 1, 1]);
});

test('the mirror answers problem+json: 404 for a repo without the app or an unknown PR, 502 when GitHub fails', async () => {
  const [noApp, unknown, down] = await Promise.all(['acme/private/pulls/1', 'acme/widgets/pulls/404', 'acme/widgets/pulls/19'].map(mirror));
  assert.deepEqual([noApp.status, noApp.type, unknown.status, unknown.type, down.status, down.type], [404, 'application/problem+json', 404, 'application/problem+json', 502, 'application/problem+json']);
  assert.equal((await fetch(`${service}/pulls?keys=acme/widgets%2310`)).status, 404, 'the keyed lookup is gone');
});

test('a GitHub outage serves a stored PR as stale, with its Age; with nothing stored it is a 502, or a 503 with Retry-After for a rate limit', async () => {
  const statesFile = join(mkdtempSync(join(tmpdir(), 'pulls-')), 'p.json');
  const checked = Date.now() - 11 * 60_000;
  writeFileSync(statesFile, JSON.stringify({ 'acme/widgets#24': { number: 24, state: 'open', merged: false, merged_at: null, closed_at: null, html_url: 'https://github.com/acme/widgets/pull/24', title: 't', checked } }));
  const svc = await listen(serveApp(createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), { ...config, statesFile })));
  const stale = await fetch(`${svc}/repos/acme/widgets/pulls/24`);
  assert.deepEqual([stale.status, ((await stale.json()) as any).state, stale.headers.get('warning')], [200, 'open', '110 - "Response is Stale"']);
  assert.ok(Number(stale.headers.get('age')) >= 660, `age ${stale.headers.get('age')}`);
  const none = await fetch(`${svc}/repos/acme/widgets/pulls/25`);
  assert.deepEqual([none.status, none.headers.get('content-type')], [502, 'application/problem+json']);
  const limited = await fetch(`${svc}/repos/acme/widgets/pulls/26`);
  assert.deepEqual([limited.status, limited.headers.get('retry-after'), limited.headers.get('content-type')], [503, '3600', 'application/problem+json']);
});

test('an oversized body is a 413 problem', async () => {
  const r = await fetch(`${service}/pulls/acme/widgets/7/reviews`, { method: 'POST', body: 'x'.repeat(6 << 20) });
  assert.deepEqual([r.status, r.headers.get('content-type')], [413, 'application/problem+json']);
});



test('the pre-contract routes are gone: POST /reviews and POST /pulls answer 404', async () => {
  const auth = { authorization: 'Bearer svc-secret' };
  const old = await fetch(`${service}/reviews`, { method: 'POST', headers: auth, body: JSON.stringify({ owner: 'acme', repo: 'widgets', number: 7, event: 'COMMENT', body: 'x', requestId: 'r' }) });
  assert.equal(old.status, 404);
  assert.equal(old.headers.get('content-type'), 'application/problem+json');
  assert.equal((await fetch(`${service}/pulls`, { method: 'POST', headers: auth, body: JSON.stringify({ owner: 'acme', repo: 'widgets', head: 'h', base: 'b', title: 't', body: '' }) })).status, 404);
  assert.equal((await fetch(`${service}/pulls/acme/widgets/7/reviews`, { headers: auth })).status, 404, 'reviews are posted, not read');
});

test('a review needs a requestId, and bodies are closed: an undeclared field is a 400 problem', async () => {
  const base = { event: 'COMMENT', body: 'x' };
  const without = await reviewAt('acme/widgets/7', base);
  assert.equal(without.status, 400);
  problemShape(await without.json(), 400);
  for (const bad of [{ requestId: '' }, { requestId: 3 }, { requestId: 'r', owner: 'acme' }, { requestId: 'r', deliveryId: 'r' }, { requestId: 'r', comments: [{ path: 'a', line: 1, body: 'b', side: 'LEFT' }] }, { requestId: 'r', comments: [{ path: 'a', body: 'b' }] }, { requestId: 'r', comments: 'x' }, { requestId: 'r', commitId: 1 }]) {
    assert.equal((await reviewAt('acme/widgets/7', { ...base, ...bad })).status, 400, JSON.stringify(bad));
  }
  assert.equal((await reviewAt('acme/widgets/7', [])).status, 400);
  assert.equal((await reviewAt('acme/widgets/7', '{not json')).status, 400, 'a body that is not JSON');
});

test('a repeat of a requestId returns the first post even when the event, body or commit differ, and posts nothing', async () => {
  const first = await postTo({ target: 'acme/widgets/15', commitId: 'abc1230000000000000000000000000000000000', event: 'REQUEST_CHANGES', body: 'first', requestId: 'conv-2/v1' });
  assert.equal(first.status, 201);
  const before = posted15.length;
  const again = await postTo({ target: 'acme/widgets/15', commitId: 'def456', event: 'APPROVE', body: 'second', requestId: 'conv-2/v1' });
  assert.deepEqual([again.status, again.body], [200, { id: first.body.id, url: first.body.url }]);
  assert.equal(posted15.length, before, 'the upstream got no second review');
});

const STALE = 'def456' + '0'.repeat(34);
const reviewPosts = () => calls.filter((c) => c.method === 'POST' && c.path.endsWith('/reviews')).length;

test('a full commitId that is not the live head is a 409 naming the live head, and nothing posts', async () => {
  const before = reviewPosts();
  const stale = await postTo({ commitId: STALE, event: 'APPROVE', body: '## Verdict\napprove' });
  assert.equal(stale.status, 409);
  problemShape(stale.body, 409);
  assert.equal(stale.body.liveHead, 'abc1230000000000000000000000000000000000');
  assert.match(stale.body.detail, /the live head is abc1230000000000000000000000000000000000$/);
  assert.equal(reviewPosts(), before, 'GitHub got no review');
});

test('a commitId that is not a full lowercase sha is a 400, and nothing posts', async () => {
  const before = reviewPosts();
  for (const commitId of ['abc123', 'ABC1230000000000000000000000000000000000', 'abc12300000000000000000000000000000000000', '']) {
    const r = await postTo({ commitId, event: 'COMMENT', body: 'x' });
    assert.equal(r.status, 400, commitId);
    problemShape(r.body, 400);
  }
  assert.equal(reviewPosts(), before);
});

test('a repeat of a posted requestId answers 200 even when its commitId is stale or short', async () => {
  const first = await postTo({ target: 'acme/widgets/15', commitId: 'abc1230000000000000000000000000000000000', event: 'COMMENT', body: 'first', requestId: 'conv-3/v1' });
  assert.equal(first.status, 201);
  for (const commitId of [STALE, 'def456']) {
    const again = await postTo({ target: 'acme/widgets/15', commitId, event: 'COMMENT', body: 'first', requestId: 'conv-3/v1' });
    assert.deepEqual([again.status, again.body], [200, first.body], commitId);
  }
});

test('a commitId naming a pull request that cannot be read is a 404 problem, and nothing posts', async () => {
  const before = reviewPosts();
  const gone = await postTo({ target: 'acme/widgets/29', commitId: STALE, event: 'APPROVE', body: '## Verdict\napprove' });
  problemShape(gone.body, 404);
  assert.equal(gone.body.detail, 'no such pull request');
  assert.equal(reviewPosts(), before, 'GitHub got no review');
});

test("GitHub's own 409 on the post passes through as a plain problem with no liveHead", async () => {
  const refused = await postTo({ target: 'acme/widgets/28', commitId: 'abc1230000000000000000000000000000000000', event: 'COMMENT', body: 'x' });
  assert.equal(refused.status, 409, 'the commitId is the live head, so the guard lets it through to GitHub');
  problemShape(refused.body, 409);
  assert.equal('liveHead' in refused.body, false);
});

test('a pull request is opened and edited under closed bodies, errors as problems', async () => {
  const auth = { authorization: 'Bearer svc-secret' };
  const open = (body: unknown, target = 'acme/widgets') => fetch(`${service}/pulls/${target}`, { method: 'POST', headers: auth, body: JSON.stringify(body) });
  const full = { head: 'h', base: 'b', title: 't', body: '' };
  assert.equal((await open(full)).status, 201, 'an empty body is declared valid');
  for (const bad of [{ ...full, owner: 'acme' }, { ...full, title: '' }, { ...full, body: undefined }, { ...full, extra: 1 }]) {
    const r = await open(bad);
    assert.equal(r.status, 400, JSON.stringify(bad));
    problemShape(await r.json(), 400);
  }
  const edit = (body: unknown) => fetch(`${service}/pulls/acme/widgets/7`, { method: 'PATCH', headers: auth, body: JSON.stringify(body) }).then((r) => r.status);
  assert.equal(await edit({ title: 't', body: 'b', number: 7 }), 400);
  assert.equal(await edit({ title: 't', body: 'b' }), 200);
  const missing = await fetch(`${service}/pulls/acme/widgets/404`, { method: 'PATCH', headers: auth, body: JSON.stringify({ title: 't', body: 'b' }) });
  assert.equal(missing.status, 404);
  problemShape(await missing.json(), 404);
});

test('an upstream outage is a 502 problem and a refusal keeps its status, as problem+json', async () => {
  const outage = await fetch(`${service}/pulls/acme/widgets/25/head`);
  assert.equal(outage.headers.get('content-type'), 'application/problem+json');
  assert.equal(outage.status, 502);
  problemShape(await outage.json(), 502);
  const limited = await fetch(`${service}/pulls/acme/widgets/26/head`);
  assert.deepEqual([limited.status, limited.headers.get('retry-after'), limited.headers.get('content-type')], [503, '3600', 'application/problem+json']);
  const refused = await postTo({ target: 'acme/widgets/18', event: 'COMMENT', body: 'x' });
  assert.equal(refused.status, 422);
  problemShape(refused.body, 422);
});
