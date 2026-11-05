import assert from 'node:assert/strict';
import { createHmac, createVerify, generateKeyPairSync } from 'node:crypto';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import { serveApp } from '@quack/sdk';
import { assertPromoted, createGithubService, githubApp, ownerRepo } from '../src/index.ts';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { fixQueued, fixRefused, noticeFor } from '../src/notice.ts';

const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048, privateKeyEncoding: { type: 'pkcs8', format: 'pem' }, publicKeyEncoding: { type: 'spki', format: 'pem' } });
const servers: Server[] = [];
after(() => servers.forEach((s) => s.close()));

async function listen(server: Server) {
  servers.push(server.listen(0));
  await new Promise((r) => server.once('listening', r));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

// A fake GitHub API that verifies the app JWT the way GitHub does: RS256 over header.body, issuer = app id.
const calls: Array<{ method: string; path: string; auth: string; body: any }> = [];
const commented: string[] = [];
const review = (id: number, login: string, state: string, body = '', commit_id = 'abc123') => ({ id, user: { login }, state, body, commit_id });
const reviews = [
  review(1, 'maria', 'CHANGES_REQUESTED', 'Handle the empty list.', 'old000'),
  review(2, 'pat', 'CHANGES_REQUESTED', 'Rename this.'),
  review(3, 'pat', 'APPROVED'),
  review(4, 'lee', 'COMMENTED', 'Nit: spacing.'),
  review(5, 'sam', 'COMMENTED'),
  review(6, 'quack-app[bot]', 'DISMISSED', 'stale'),
];
const thread = (isResolved: boolean, path: string, body: string) => ({ isResolved, path, line: 4, originalLine: 4, comments: { nodes: [{ body, author: { login: 'maria' } }] } });
const threads = [thread(false, 'app.ts', 'Off by one.'), thread(true, 'old.ts', 'Fixed already.')];
const commentWaiters: Array<() => void> = [];
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
    calls.push({ method: req.method!, path: req.url!, auth, body: raw ? JSON.parse(raw) : undefined });
    const reply = (status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
    // GitHub's API is case-insensitive: a mixed-case owner reaches the same repository.
    const url = req.url!.replace(/^\/repos\/ACME\//, '/repos/acme/');
    if (url === '/app') return jwtValid(auth) ? reply(200, { slug: 'quack-app' }) : reply(401, {});
    if (url === '/repos/acme/widgets/installation') return jwtValid(auth) ? reply(200, { id: 42 }) : reply(401, {});
    if (url === '/repos/acme/private/installation') return reply(404, { message: 'Not Found' });
    if (url === '/app/installations/42/access_tokens') return jwtValid(auth) ? reply(201, { token: 'ghs_test', expires_at: new Date(Date.now() + 3600_000).toISOString() }) : reply(401, {});
    if (url === '/repos/acme/widgets/pulls/7' && auth === 'token ghs_test') return reply(200, { number: 7, title: 't', body: 'Fixes #3', user: { login: 'dev' }, head: { sha: 'abc123', ref: 'fix' }, base: { ref: 'main' } });
    if (url === '/repos/acme/widgets/pulls/9') return reply(200, { number: 9, state: 'closed', merged: true, merged_at: '2026-11-01T00:00:00Z' });
    if (url === '/repos/acme/widgets/pulls/10') return reply(200, { number: 10, state: 'closed', merged: false, closed_at: '2026-11-02T00:00:00Z' });
    if (url === '/repos/acme/widgets/pulls/11') return reply(200, { number: 11, state: 'open', updated_at: '2026-11-03T00:00:00Z' });
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
    const page = /^\/repos\/acme\/widgets\/commits\/abc123\/check-runs\?per_page=100&page=(\d+)$/.exec(url);
    // 150 check runs: the only failing one is on the second page.
    if (page) return reply(200, { check_runs: Array.from({ length: page[1] === '1' ? 100 : 50 }, (_, i) => ({ name: `c${page[1]}-${i}`, status: 'completed', conclusion: page[1] === '2' && i === 49 ? 'failure' : 'success' })) });
    if (url === '/repos/acme/widgets/pulls/7/comments/55/replies') return reply(201, { id: 56, html_url: 'https://github.com/acme/widgets/pull/7#discussion_r56' });
    if (url === '/repos/acme/widgets/issues/comments/60') return reply(200, { body: '@quack-app why is this cached?\nIt looks wrong.' });
    // GitHub refuses a whole review when one inline line is off the diff.
    if (url === '/repos/acme/widgets/pulls/7/reviews' && calls.at(-1)!.body?.comments?.some((c: any) => c.path === 'gone.ts')) return reply(422, { message: 'Line could not be resolved' });
    if (url === '/repos/acme/widgets/pulls/7/reviews' && calls.at(-1)!.body?.comments?.some((c: any) => c.path === 'down.ts')) return reply(503, { message: 'unavailable' });
    if (url === '/repos/acme/widgets/pulls/7/reviews') return auth === 'token ghs_test' ? reply(200, { id: 9, html_url: 'https://github.com/acme/widgets/pull/7#review-9' }) : reply(401, {});
    if (url === '/repos/acme/widgets/pulls/7/reviews?per_page=100') return reply(200, reviews);
    if (url === '/graphql') return reply(200, { data: { repository: { pullRequest: { reviewThreads: { nodes: threads } } } } });
    reply(404, {});
  }),
);

const invocations: Array<{ path: string; body: any }> = [];
const watchers: string[] = [];
const watcherWaiters: Array<() => void> = [];
const holds: Record<string, Promise<void>> = {};
let busy = false;
let busyDetail = 'session is mid-run as agent reviewer; implementer can start once it finishes';
const outcomes: Record<string, unknown> = {};
const distributor = await listen(
  createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    if (req.url === '/agents') return res.writeHead(200).end(JSON.stringify({ agents: [{ name: 'reviewer', version: 1, status: 'promoted' }, { name: 'researcher', version: 1, status: 'draft' }] }));
    const body = JSON.parse(raw);
    if (body.wait) {
      watchers.push(body.requestId);
      watcherWaiters.splice(0).forEach((f) => f());
      await holds[body.requestId];
      return res.writeHead(200).end(JSON.stringify(outcomes[body.requestId] ?? {}));
    }
    if (busy) return res.writeHead(409, { 'content-type': 'application/problem+json' }).end(JSON.stringify({ detail: busyDetail }));
    invocations.push({ path: decodeURIComponent(req.url!), body });
    res.writeHead(202).end('{}');
  }),
);

const config = { webhookSecret: 'hook-secret', distributorUrl: distributor, label: 'quack:review', agent: 'reviewer' };
const service = await listen(serveApp(createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), config)));

function deliver(event: string, payload: unknown, opts: { secret?: string; id?: string } = {}) {
  const body = JSON.stringify(payload);
  const sig = `sha256=${createHmac('sha256', opts.secret ?? 'hook-secret').update(body).digest('hex')}`;
  return fetch(`${service}/webhook`, { method: 'POST', body, headers: { 'x-github-event': event, 'x-hub-signature-256': sig, 'x-github-delivery': opts.id ?? 'd-1' } }).then((r) => r.status);
}

const repository = { name: 'widgets', owner: { login: 'acme' } };
const pr = { number: 7, title: 'Fix the cache', body: 'Closes #3', head: { sha: 'abc123', ref: 'fix' }, base: { ref: 'main' } };

test('verified webhooks start a review in the PR session, keyed by the delivery id', async () => {
  invocations.length = 0;
  assert.equal(await deliver('pull_request', { action: 'labeled', label: { name: 'quack:review' }, pull_request: pr, repository }, { secret: 'wrong' }), 401);
  assert.equal(await deliver('pull_request', { action: 'labeled', label: { name: 'bug' }, pull_request: pr, repository }), 200);
  assert.equal(await deliver('pull_request', { action: 'labeled', label: { name: 'quack:review' }, pull_request: pr, repository }, { id: 'd-7' }), 202);
  assert.equal(invocations.length, 1);
  assert.equal(invocations[0].path, '/sessions/github:acme/widgets#7/invocations?agent=reviewer');
  assert.equal(invocations[0].body.requestId, 'github-delivery:d-7');
  assert.match(invocations[0].body.input, /Review pull request acme\/widgets#7 \(label quack:review\)/);
  assert.match(invocations[0].body.input, /Head: abc123 on fix; base: main/);
  assert.equal(invocations[0].body.title, 'Fix the cache');
  // The opening card's data rides in meta, keyed by head sha; the model's input text carries none of it.
  assert.deepEqual(invocations[0].body.meta.files, [{ path: 'src/a.ts', additions: 5, deletions: 2 }]);
  assert.deepEqual(invocations[0].body.meta.linked, [{ number: 3, title: 'bug' }]);
  assert.equal(invocations[0].body.meta.id, 'abc123');
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
const fixService = (extra: object = {}) => {
  const svc = createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), { ...config, implementerAgent: 'implementer', queuePollMs: 10, ...extra });
  return listen(serveApp(svc)).then((url) => ({ svc, url }));
};
const postFix = (url: string, id: string) => {
  const body = JSON.stringify(fixComment);
  const sig = `sha256=${createHmac('sha256', 'hook-secret').update(body).digest('hex')}`;
  return fetch(`${url}/webhook`, { method: 'POST', body, headers: { 'x-github-event': 'issue_comment', 'x-hub-signature-256': sig, 'x-github-delivery': id } }).then((r) => r.status);
};

test('a /fix that finds the session mid-run is queued behind it, answered once, and started once the session is idle', async () => {
  invocations.length = 0;
  commented.length = 0;
  const { svc, url } = await fixService();
  try {
    busy = true;
    assert.equal(await postFix(url, 'd-q1'), 200);
    assert.equal(await postFix(url, 'd-q1'), 200, 'a redelivery');
    await sleep(60);
    assert.deepEqual(commented, [fixQueued]);
    assert.equal(invocations.length, 0);
    busy = false;
    while (!invocations.length) await sleep(10);
    await sleep(60);
    assert.equal(invocations.length, 1, 'started once');
    assert.equal(invocations[0].body.requestId, 'github-delivery:d-q1');
    assert.equal(invocations[0].path, '/sessions/github:acme/widgets#7/invocations?agent=implementer');
    assert.match(invocations[0].body.input, /Instruction: the cache test/);
    assert.equal(invocations[0].body.meta.id, 'abc123');
  } finally {
    busy = false;
    svc.stop();
  }
});

test('a queued /fix survives a restart: a new service reading the same file starts it', async () => {
  invocations.length = 0;
  commented.length = 0;
  const queueFile = join(mkdtempSync(join(tmpdir(), 'fixq-')), 'github-fixes.json');
  const first = await fixService({ queueFile });
  busy = true;
  assert.equal(await postFix(first.url, 'd-q2'), 200);
  first.svc.stop();
  busy = false;
  await sleep(60);
  assert.equal(invocations.length, 0, 'nothing retries without the timer');
  const second = await fixService({ queueFile });
  try {
    second.svc.resume();
    while (!invocations.length) await sleep(10);
    await sleep(60);
    assert.equal(invocations.length, 1);
    assert.equal(invocations[0].body.requestId, 'github-delivery:d-q2');
    assert.deepEqual(JSON.parse(readFileSync(queueFile, 'utf8')), {}, 'started work leaves the file');
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
    busy = false;
    await sleep(60);
    assert.equal(invocations.length, 0);
    assert.deepEqual(commented, [fixRefused('agent implementer is not registered')]);
  } finally {
    busy = false;
    busyDetail = 'session is mid-run as agent reviewer; implementer can start once it finishes';
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

test('a pull request is opened with the installation token, mentions of the bot defanged', async () => {
  const open = (body: unknown) => fetch(`${service}/pulls`, { method: 'POST', body: JSON.stringify(body), headers: { authorization: 'Bearer svc-secret' } }).then(async (r) => ({ status: r.status, body: await r.json() }));
  assert.equal((await open({ owner: 'acme', repo: 'widgets', head: 'quack/12-x', base: 'main', title: 't' })).status, 400);
  calls.length = 0;
  const made = await open({ owner: 'acme', repo: 'widgets', head: 'quack/12-x', base: 'trunk', title: 'feat: x', body: 'ping @quack-app\n\nCloses #12' });
  assert.deepEqual(made, { status: 201, body: { number: 13, url: 'https://github.com/acme/widgets/pull/13' } });
  const sent = calls.find((c) => c.path === '/repos/acme/widgets/pulls')!;
  assert.equal(sent.auth, 'token ghs_test');
  assert.deepEqual(sent.body, { head: 'quack/12-x', base: 'trunk', title: 'feat: x', body: 'ping quack-app\n\nCloses #12' });
});

test('git credentials are minted per repository and cached until near expiry', async () => {
  const get = (url: string, token = 'svc-secret') =>
    fetch(`${service}/git-credential?url=${encodeURIComponent(url)}`, { headers: { authorization: `Bearer ${token}` } }).then(async (r) => ({ status: r.status, body: await r.json() }));
  calls.length = 0;
  assert.deepEqual((await get('https://github.com/acme/widgets.git')).body, { username: 'x-access-token', token: 'ghs_test' });
  assert.equal((await get('https://github.com/acme/widgets')).status, 200);
  // The webhook test above may already have minted it; either way two requests mint at most once.
  assert.ok(calls.filter((c) => c.path.endsWith('/access_tokens')).length <= 1, 'the second request uses the cached token');
  assert.equal((await get('https://gitlab.com/acme/widgets.git')).status, 404);
  assert.equal((await get('https://github.com/acme/private.git')).status, 404, 'the app is not installed there');
});

test('a sealed review is posted with the installation token, never handed to the session', async () => {
  const post = (body: unknown) =>
    fetch(`${service}/reviews`, { method: 'POST', body: JSON.stringify(body), headers: { authorization: 'Bearer svc-secret' } }).then(async (r) => ({ status: r.status, body: await r.json() }));
  assert.equal((await post({ owner: 'acme', repo: 'widgets', number: 7, event: 'MERGE', body: 'x' })).status, 400);
  const posted = await post({ owner: 'acme', repo: 'widgets', number: 7, commitId: 'abc123', event: 'REQUEST_CHANGES', body: '## Verdict\nrequest_changes' });
  assert.equal(posted.status, 201);
  assert.equal(posted.body.url, 'https://github.com/acme/widgets/pull/7#review-9');
  const sent = calls.findLast((c) => c.path.endsWith('/reviews'))!;
  assert.deepEqual(sent.body, { event: 'REQUEST_CHANGES', body: '## Verdict\nrequest_changes', commit_id: 'abc123' });
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
});

test('a commit with more check runs than the service reads is refused, not reviewed against part of its CI', async () => {
  const res = await fetch(`${service}/pulls/acme/widgets/8`, { headers: { authorization: 'Bearer svc-secret' } });
  assert.equal(res.status, 502);
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

test('a redelivered event carries the original delivery id, so it is one submission', async () => {
  invocations.length = 0;
  const event = { action: 'labeled', label: { name: 'quack:review' }, pull_request: pr, repository };
  assert.equal(await deliver('pull_request', event, { id: 'd-redo' }), 202);
  assert.equal(await deliver('pull_request', event, { id: 'd-redo' }), 202);
  assert.deepEqual(invocations.map((i) => i.body.requestId), ['github-delivery:d-redo', 'github-delivery:d-redo']);
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
  const review = { owner: 'acme', repo: 'widgets', number: 7, commitId: 'abc123', event: 'APPROVE', body: '## Verdict\napprove' };
  assert.equal((await fetch(`${service}/reviews`, { method: 'POST', headers: { authorization: 'Bearer svc-secret' }, body: JSON.stringify(review) })).status, 201);
  release();
  outcomes[`github-delivery:${id}-sentinel`] = { status: 'unanswered', reason: 'run_failed' };
  const noted = new Promise<void>((r) => commentWaiters.push(r));
  assert.equal(await deliver('pull_request', { ...labeled, repository }, { id: `${id}-sentinel` }), 202);
  await noted;
  assert.deepEqual(commented, ['The review could not be completed (run_failed). Comment `/review` to try again.']);
}

test('a run that delivered a review gets no notice', () => deliveredRunIsSilent('acme', 'd-delivered'));

test('a review counts for the run however the webhook cases the owner', () => deliveredRunIsSilent('ACME', 'd-delivered-case'));

const mention = (body: string, extra: Record<string, unknown> = {}) => ({ action: 'created', issue: { ...pr, pull_request: {} }, comment: { id: 60, body, html_url: 'https://github.com/acme/widgets/pull/7#issuecomment-60', author_association: 'MEMBER', user: { login: 'dev', type: 'User' }, ...extra }, repository });

test('a trusted mention is queued behind the PR session\'s run with its text, author and link', async () => {
  invocations.length = 0;
  assert.equal(await deliver('issue_comment', mention('@QUACK-APP why is this cached?'), { id: 'd-m1' }), 202);
  const [sent] = invocations;
  assert.equal(sent.path, '/sessions/github:acme/widgets#7/invocations?agent=reviewer');
  assert.equal(sent.body.whenBusy, 'followUp');
  assert.equal(sent.body.requestId, 'github-delivery:d-m1');
  assert.match(sent.body.input, /^dev mentioned you in a comment on acme\/widgets#7: https:\/\/github.com\/acme\/widgets\/pull\/7#issuecomment-60/);
  assert.match(sent.body.input, /reply_mention with repository acme\/widgets, number 7, kind issue, commentId 60/);
  assert.match(sent.body.input, /@QUACK-APP why is this cached\?$/);
  assert.equal(sent.body.meta, undefined);
});

test('a review-comment mention names the thread root, so the reply lands under it', async () => {
  invocations.length = 0;
  const payload = { action: 'created', pull_request: pr, comment: { id: 56, in_reply_to_id: 55, body: 'thanks @quack-app, and the fix?', html_url: 'https://github.com/acme/widgets/pull/7#discussion_r56', author_association: 'OWNER', user: { login: 'dev', type: 'User' } }, repository };
  assert.equal(await deliver('pull_request_review_comment', payload, { id: 'd-m2' }), 202);
  assert.match(invocations[0].body.input, /review comment thread/);
  assert.match(invocations[0].body.input, /kind review, commentId 55/);
});

test('an untrusted, self-authored or token-less comment is ignored', async () => {
  invocations.length = 0;
  assert.equal(await deliver('issue_comment', mention('@quack-app hi', { author_association: 'NONE' })), 200);
  assert.equal(await deliver('issue_comment', mention('@quack-app hi', { user: { login: 'Quack-App[bot]', type: 'User' } })), 200);
  assert.equal(await deliver('issue_comment', mention('@quack-app hi', { user: { login: 'other-app[bot]', type: 'Bot' } })), 200);
  assert.equal(await deliver('issue_comment', mention('@quack hi, @quack-apple, email me@quack-app.dev')), 200);
  assert.equal(await deliver('issue_comment', mention('@quack-app hi'), { secret: 'wrong' }), 401);
  assert.equal(invocations.length, 0);
});

test('replies post in the right thread and strip its own handle outside code', async () => {
  const reply = (body: unknown) => fetch(`${service}/replies`, { method: 'POST', body: JSON.stringify(body), headers: { authorization: 'Bearer svc-secret' } }).then(async (r) => ({ status: r.status, body: await r.json() }));
  assert.equal((await reply({ owner: 'acme', repo: 'widgets', number: 7, kind: 'nope', commentId: 1, body: 'x' })).status, 400);
  calls.length = 0;
  const inThread = await reply({ owner: 'acme', repo: 'widgets', number: 7, kind: 'review', commentId: 55, body: 'Ask @alice and @quack-app; keep `@quack-app`.' });
  assert.equal(inThread.status, 201);
  assert.deepEqual(calls.findLast((c) => c.path.endsWith('/replies'))!.body, { body: 'Ask @alice and quack-app; keep `@quack-app`.' });
  commented.length = 0;
  await reply({ owner: 'acme', repo: 'widgets', number: 7, kind: 'issue', commentId: 60, body: 'It is cached for speed, @dev.' });
  assert.equal(commented[0], '> quack-app why is this cached?\n> It looks wrong.\n\nIt is cached for speed, @dev.');
});

test('a review posts its inline comments on the right side, mentions stripped; a 422 refusal posts the review as written, an outage is not retried', async () => {
  const post = (comments: unknown[]) => fetch(`${service}/reviews`, { method: 'POST', body: JSON.stringify({ owner: 'acme', repo: 'widgets', number: 7, commitId: 'abc123', event: 'COMMENT', body: 'cc @quack-app', comments }), headers: { authorization: 'Bearer svc-secret' } }).then((r) => r.status);
  assert.equal(await post([{ path: 'src/a.ts', line: 3, body: '**nit**: ping @quack-app' }]), 201);
  assert.deepEqual(calls.findLast((c) => c.path.endsWith('/reviews'))!.body, { event: 'COMMENT', body: 'cc quack-app', commit_id: 'abc123', comments: [{ path: 'src/a.ts', line: 3, side: 'RIGHT', body: '**nit**: ping quack-app' }] });
  const refused = [{ path: 'gone.ts', line: 3, body: '**nit**: lost?' }];
  const posts = () => calls.filter((c) => c.path.endsWith('/reviews')).length;
  const before = posts();
  const fallback = await fetch(`${service}/reviews`, { method: 'POST', body: JSON.stringify({ owner: 'acme', repo: 'widgets', number: 7, event: 'COMMENT', body: 'listed inline', fallbackBody: 'as written, @quack-app', comments: refused }), headers: { authorization: 'Bearer svc-secret' } });
  assert.equal(fallback.status, 201);
  assert.equal(posts() - before, 2);
  assert.deepEqual(calls.findLast((c) => c.path.endsWith('/reviews'))!.body, { event: 'COMMENT', body: 'as written, quack-app' });
  // An outage is not a refusal: one attempt, a 502 to the session, nothing marked posted.
  const down = await fetch(`${service}/reviews`, { method: 'POST', body: JSON.stringify({ owner: 'acme', repo: 'widgets', number: 7, event: 'COMMENT', body: 'x', fallbackBody: 'y', comments: [{ path: 'down.ts', line: 1, body: 'n' }] }), headers: { authorization: 'Bearer svc-secret' } });
  assert.equal(down.status, 502);
  assert.equal(posts() - before, 3);
});

const pulls = (keys: string, token = 'svc-secret') =>
  fetch(`${service}/pulls?keys=${encodeURIComponent(keys)}`, { headers: { authorization: `Bearer ${token}` } }).then(async (r) => ({ status: r.status, body: (await r.json()) as any }));
const reads = (n: number) => calls.filter((c) => c.path === `/repos/acme/widgets/pulls/${n}`).length;

test('closed and reopened webhooks record the PR state and start no run', async () => {
  invocations.length = 0;
  const closed = (n: number, merged: boolean) => ({ action: 'closed', pull_request: { ...pr, number: n, state: 'closed', merged, merged_at: merged ? '2026-11-03T10:00:00Z' : null, closed_at: '2026-11-03T10:00:00Z', labels: [{ name: 'quack:review' }] }, repository });
  assert.equal(await deliver('pull_request', closed(20, true), { id: 'd-c1' }), 200);
  assert.equal(await deliver('pull_request', closed(21, false), { id: 'd-c2' }), 200);
  assert.equal(await deliver('pull_request', { action: 'reopened', pull_request: { ...pr, number: 22, state: 'open' }, repository }, { id: 'd-c3' }), 200);
  assert.equal(invocations.length, 0);
  const closedThenReopened = [closed(23, false), { action: 'reopened', pull_request: { ...pr, number: 23, state: 'open' }, repository }];
  for (const [i, p] of closedThenReopened.entries()) await deliver('pull_request', p, { id: `d-r${i}` });
  const before = calls.length;
  const { body } = await pulls('github:acme/widgets#20,ACME/widgets#21,acme/widgets#22,acme/widgets#23');
  assert.equal(calls.length, before, 'recorded states need no GitHub read');
  const states = Object.fromEntries(Object.entries(body.pulls).map(([k, v]: any) => [k, v.state]));
  assert.deepEqual(states, { 'acme/widgets#20': 'merged', 'ACME/widgets#21': 'closed', 'acme/widgets#22': 'open', 'acme/widgets#23': 'open' });
  assert.equal(body.pulls['acme/widgets#20'].at, '2026-11-03T10:00:00Z');
});

test('an unseen PR is read from GitHub once; merged and closed stay cached', async () => {
  const keys = 'github:acme/widgets#9,github:acme/widgets#10,github:acme/widgets#11';
  const first = await pulls(keys);
  assert.deepEqual(Object.fromEntries(Object.entries(first.body.pulls).map(([k, v]: any) => [k, v.state])), { 'acme/widgets#9': 'merged', 'acme/widgets#10': 'closed', 'acme/widgets#11': 'open' });
  await pulls(keys);
  assert.deepEqual([reads(9), reads(10), reads(11)], [1, 1, 1]);
});

test('an oversized body is a 413 problem', async () => {
  const r = await fetch(`${service}/reviews`, { method: 'POST', body: 'x'.repeat(6 << 20) });
  assert.deepEqual([r.status, r.headers.get('content-type')], [413, 'application/problem+json']);
});

test('a PR the app cannot read is left out', async () => {
  assert.deepEqual((await pulls('github:acme/private#1,not-a-key')).body, { pulls: {} });
});
