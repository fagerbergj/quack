import assert from 'node:assert/strict';
import { createHmac, createVerify, generateKeyPairSync } from 'node:crypto';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import { assertPromoted, createGithubService, githubApp, ownerRepo } from '../src/index.ts';

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
    if (req.url === '/repos/acme/widgets/installation') return jwtValid(auth) ? reply(200, { id: 42 }) : reply(401, {});
    if (req.url === '/repos/acme/private/installation') return reply(404, { message: 'Not Found' });
    if (req.url === '/app/installations/42/access_tokens') return jwtValid(auth) ? reply(201, { token: 'ghs_test', expires_at: new Date(Date.now() + 3600_000).toISOString() }) : reply(401, {});
    if (req.url === '/repos/acme/widgets/pulls/7' && auth === 'token ghs_test') return reply(200, { number: 7, title: 't', body: 'Fixes #3', user: { login: 'dev' }, head: { sha: 'abc123', ref: 'fix' }, base: { ref: 'main' } });
    if (req.url === '/repos/acme/widgets/pulls/8' && auth === 'token ghs_test') return reply(200, { number: 8, title: 't', body: '', user: { login: 'dev' }, head: { sha: 'full111', ref: 'big' }, base: { ref: 'main' } });
    // Every page full: a commit whose check runs never end.
    if (req.url?.startsWith('/repos/acme/widgets/commits/full111/check-runs')) return reply(200, { check_runs: Array.from({ length: 100 }, (_, i) => ({ name: `c${i}`, status: 'completed', conclusion: 'success' })) });
    if (req.url === '/repos/acme/widgets/issues/3') return reply(200, { number: 3, title: 'bug', body: 'b' });
    const page = /^\/repos\/acme\/widgets\/commits\/abc123\/check-runs\?per_page=100&page=(\d+)$/.exec(req.url!);
    // 150 check runs: the only failing one is on the second page.
    if (page) return reply(200, { check_runs: Array.from({ length: page[1] === '1' ? 100 : 50 }, (_, i) => ({ name: `c${page[1]}-${i}`, status: 'completed', conclusion: page[1] === '2' && i === 49 ? 'failure' : 'success' })) });
    if (req.url === '/repos/acme/widgets/pulls/7/reviews') return auth === 'token ghs_test' ? reply(200, { id: 9, html_url: 'https://github.com/acme/widgets/pull/7#review-9' }) : reply(401, {});
    reply(404, {});
  }),
);

const invocations: Array<{ path: string; body: any }> = [];
const distributor = await listen(
  createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    if (req.url === '/agents') return res.writeHead(200).end(JSON.stringify({ agents: [{ name: 'reviewer', version: 1, status: 'promoted' }, { name: 'researcher', version: 1, status: 'draft' }] }));
    invocations.push({ path: decodeURIComponent(req.url!), body: JSON.parse(raw) });
    res.writeHead(202).end('{}');
  }),
);

const config = { webhookSecret: 'hook-secret', serviceToken: 'svc-secret', distributorUrl: distributor, label: 'quack:review', agent: 'reviewer' };
const service = await listen(createGithubService(githubApp({ appId: '123', privateKey, apiUrl: github }), config));

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
});

test('/review starts a review only from a trusted commenter', async () => {
  invocations.length = 0;
  const comment = (association: string) => ({ action: 'created', issue: { ...pr, pull_request: {} }, comment: { body: ' /review ', author_association: association, user: { login: 'someone' } }, repository });
  assert.equal(await deliver('issue_comment', comment('CONTRIBUTOR')), 200);
  assert.equal(await deliver('issue_comment', comment('MEMBER'), { id: 'd-8' }), 202);
  assert.equal(invocations.length, 1);
  assert.match(invocations[0].body.input, /\/review by someone/);
});

test('git credentials are minted per repository and cached until near expiry', async () => {
  const get = (url: string, token = 'svc-secret') =>
    fetch(`${service}/git-credential?url=${encodeURIComponent(url)}`, { headers: { authorization: `Bearer ${token}` } }).then(async (r) => ({ status: r.status, body: await r.json() }));
  calls.length = 0;
  assert.equal((await get('https://github.com/acme/widgets.git', 'nope')).status, 401);
  assert.deepEqual((await get('https://github.com/acme/widgets.git')).body, { username: 'x-access-token', token: 'ghs_test' });
  assert.equal((await get('https://github.com/acme/widgets')).status, 200);
  assert.equal(calls.filter((c) => c.path.endsWith('/access_tokens')).length, 1, 'the second request uses the cached token');
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

test('a commit with more check runs than the service reads is refused, not reviewed against part of its CI', async () => {
  const res = await fetch(`${service}/pulls/acme/widgets/8`, { headers: { authorization: 'Bearer svc-secret' } });
  assert.equal(res.status, 502);
  assert.match((await res.json()).error, /more than 1000 check runs on full111/);
});

test('only a promoted agent can back the unattended trigger', async () => {
  await assertPromoted(distributor, 'reviewer');
  await assert.rejects(assertPromoted(distributor, 'researcher'), /researcher is draft/);
  await assert.rejects(assertPromoted(distributor, 'ghost'), /not registered/);
  await assert.rejects(assertPromoted('http://127.0.0.1:1', 'reviewer'), /cannot verify agent promotion/);
});

test('a redelivered event carries the original delivery id, so it is one submission', async () => {
  invocations.length = 0;
  const event = { action: 'labeled', label: { name: 'quack:review' }, pull_request: pr, repository };
  assert.equal(await deliver('pull_request', event, { id: 'd-redo' }), 202);
  assert.equal(await deliver('pull_request', event, { id: 'd-redo' }), 202);
  assert.deepEqual(invocations.map((i) => i.body.requestId), ['github-delivery:d-redo', 'github-delivery:d-redo']);
});
