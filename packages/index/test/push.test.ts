import assert from 'node:assert/strict';
import { mkdtemp, rm, stat } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createStore, indexRoutes, openPush, sqliteBackend, type Fact } from '../src/index.ts';

const dirs: string[] = [];
after(async () => { for (const d of dirs) await rm(d, { recursive: true, force: true }); });
const NOW = 1_000_000_000;
const sub = (n: number) => ({ endpoint: `https://push.example/${n}`, keys: { p256dh: 'p', auth: 'a' } });

async function setup(failWith?: (endpoint: string) => number | undefined) {
  const dir = await mkdtemp(join(tmpdir(), 'quack-push-'));
  dirs.push(dir);
  const sent: Array<{ endpoint: string; payload: { title: string; body: string; url: string } }> = [];
  const push = openPush({
    dataDir: dir, now: () => NOW, title: async () => 'Review #2095',
    send: async (s, payload) => {
      const statusCode = failWith?.(s.endpoint);
      if (statusCode) throw Object.assign(new Error('push service said no'), { statusCode });
      sent.push({ endpoint: s.endpoint, payload: JSON.parse(payload) });
    },
  });
  const server = createServer((req, res) => void push.routes(req, res));
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const post = (path: string, body: unknown) => fetch(url + path, { method: 'POST', body: JSON.stringify(body) });
  after(() => { server.close(); });
  return { dir, push, sent, post, url };
}

const approval = (id: string, state = 'pending', at = NOW): Fact => ({ id: `approval:${id}`, kind: 'approval', rev: state === 'pending' ? 1 : 2, at, data: { tool: 'post', state } });

test('a new pending approval sends one plain-words push to each subscription, and a replay sends none', async () => {
  const { push, sent, post } = await setup();
  await post('/push/subscribe', { subscription: sub(1) });
  await post('/push/subscribe', { subscription: sub(2) });
  await push.notify('github:o/r#1', [approval('a1')]);
  await push.notify('github:o/r#1', [approval('a1')]);
  assert.deepEqual(sent.map((s) => s.endpoint).sort(), ['https://push.example/1', 'https://push.example/2']);
  assert.deepEqual(sent[0]!.payload, { title: 'Approval waiting: Review #2095', body: '', url: '#/s/github%3Ao%2Fr%231' });
});

test('what was sent survives a restart, and resolved or stale facts send nothing', async () => {
  const { dir, push, sent, post } = await setup();
  await post('/push/subscribe', { subscription: sub(1) });
  await push.notify('s', [approval('a1')]);
  push.close();
  const again = openPush({ dataDir: dir, now: () => NOW, send: async (_s, p) => void sent.push({ endpoint: '', payload: JSON.parse(p) }) });
  await again.notify('s', [approval('a1'), approval('a2', 'resolved'), approval('a3', 'pending', NOW - 7 * 3600_000)]);
  again.close();
  assert.equal(sent.length, 1);
});

test('questions, failures and sealed reports each push once; a later failed run pushes again', async () => {
  const { push, sent, post } = await setup();
  await post('/push/subscribe', { subscription: sub(1) });
  const question: Fact = { id: 'question:q1', kind: 'question', rev: 1, at: NOW, data: { question: 'Ship  it?\nnow', state: 'pending' } };
  const failure = (rev: number): Fact => ({ id: 'failure:step', kind: 'failure', rev, at: NOW, data: { step: 'step', error: 'boom', state: 'failed' } });
  const report = artifact('review', 1, { passed: true });
  await push.notify('s', [question, failure(4), report, failure(4), failure(6), { ...report, rev: 5 }]);
  assert.deepEqual(sent.map((s) => [s.payload.title, s.payload.body]), [
    ['Question waiting: Review #2095', 'Ship it? now'], ['Session failed: Review #2095', ''], ['Session failed: Review #2095', ''], ['Report ready: Review #2095', ''],
  ]);
});

const artifact = (kind: string, version: number, verdict: { passed: boolean; limitReached?: boolean } | null): Fact => ({ id: `artifact:${kind}:${version}`, kind: 'artifact', rev: verdict ? 3 : 1, at: NOW, data: { kind, version, verdict } });
const idle: Fact = { id: 'status', kind: 'status', rev: 2, at: NOW, data: { state: 'idle' } };

test('a failed draft is not a report; the passing revision is, once', async () => {
  const { push, sent, post } = await setup();
  await post('/push/subscribe', { subscription: sub(1) });
  await push.notify('s', [artifact('review', 1, null)]);
  await push.notify('s', [artifact('review', 1, { passed: false }), idle]);
  await push.notify('s', [artifact('review', 2, null)]);
  await push.notify('s', [artifact('review', 2, { passed: true }), idle]);
  await push.notify('s', [artifact('review', 2, { passed: true }), idle]);
  assert.deepEqual(sent.map((s) => s.payload.title), ['Report ready: Review #2095']);
});

test('a version released at the revision limit is a report', async () => {
  const { push, sent, post } = await setup();
  await post('/push/subscribe', { subscription: sub(1) });
  await push.notify('s', [artifact('review', 3, { passed: false, limitReached: true })]);
  assert.equal(sent.length, 1);
});

test('an unjudged kind pushes its newest version when the run ends, once per version', async () => {
  const stored = [{ kind: 'cards', version: 1, verdict: false }, { kind: 'cards', version: 2, verdict: false }, { kind: 'judged', version: 1, verdict: true }];
  const dir = await mkdtemp(join(tmpdir(), 'quack-push-'));
  dirs.push(dir);
  const sent: string[] = [];
  const push = openPush({ dataDir: dir, now: () => NOW, artifacts: async () => stored, send: async (_s, p) => void sent.push(JSON.parse(p).title) });
  const server = createServer((req, res) => void push.routes(req, res));
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  await fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/push/subscribe`, { method: 'POST', body: JSON.stringify({ subscription: sub(1) }) });
  server.close();
  await push.notify('s', [artifact('cards', 1, null), artifact('cards', 2, null)]);
  assert.deepEqual(sent, []);
  await push.notify('s', [idle]);
  await push.notify('s', [{ ...idle, rev: 4 }]);
  stored.push({ kind: 'cards', version: 3, verdict: false });
  await push.notify('s', [{ ...idle, rev: 6 }]);
  assert.deepEqual(sent, ['Report ready', 'Report ready']);
  push.close();
});

test('a 410 or 404 from the push service removes the subscription, other errors keep it', async () => {
  const { push, sent, post } = await setup((e) => (e.endsWith('/1') ? 410 : e.endsWith('/2') ? 404 : e.endsWith('/3') ? 500 : undefined));
  for (const n of [1, 2, 3, 4]) await post('/push/subscribe', { subscription: sub(n) });
  const quiet = console.error;
  console.error = () => {};
  try {
    await push.notify('s', [approval('a1')]);
    await push.notify('s', [approval('a2')]);
  } finally {
    console.error = quiet;
  }
  assert.deepEqual(sent.map((s) => s.endpoint), ['https://push.example/4', 'https://push.example/4']);
});

test('subscribe validates, unsubscribe removes, test pushes to the asking device only, and the keys persist', async () => {
  const { dir, push, sent, post, url } = await setup();
  assert.equal((await post('/push/subscribe', { subscription: { endpoint: 'http://insecure/1', keys: { p256dh: 'p', auth: 'a' } } })).status, 400);
  assert.equal((await post('/push/test', { endpoint: sub(1).endpoint })).status, 404);
  await post('/push/subscribe', { subscription: sub(1) });
  await post('/push/subscribe', { subscription: sub(2) });
  assert.equal((await post('/push/test', { endpoint: sub(1).endpoint })).status, 200);
  assert.deepEqual(sent.map((s) => s.endpoint), ['https://push.example/1']);
  await post('/push/unsubscribe', { endpoint: sub(1).endpoint });
  await push.notify('s', [approval('a1')]);
  assert.deepEqual(sent.map((s) => s.endpoint), ['https://push.example/1', 'https://push.example/2']);
  const { publicKey } = (await (await fetch(`${url}/push/key`)).json()) as { publicKey: string };
  assert.ok(publicKey.length > 40);
  assert.equal((await stat(join(dir, 'vapid.json'))).mode & 0o077, 0);
  const reopened = openPush({ dataDir: dir });
  const again = createServer((req, res) => void reopened.routes(req, res));
  await new Promise<void>((r) => again.listen(0, '127.0.0.1', r));
  const key2 = (await (await fetch(`http://127.0.0.1:${(again.address() as AddressInfo).port}/push/key`)).json()) as { publicKey: string };
  again.close();
  reopened.close();
  assert.equal(key2.publicKey, publicKey);
});

test('the index calls onFacts with each accepted publish', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-push-'));
  dirs.push(dir);
  const seen: string[] = [];
  const store = createStore(sqliteBackend(join(dir, 'index.sqlite')));
  const handler = indexRoutes(store, (t) => (t === 't' ? 's1' : undefined), async (session, facts) => void seen.push(`${session}:${facts.length}`));
  const server = createServer((req, res) => void handler(req, res));
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const res = await fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/index/facts`, { method: 'POST', headers: { authorization: 'Bearer t' }, body: JSON.stringify({ session: 's1', facts: [approval('a1')] }) });
  server.close();
  assert.equal(res.status, 200);
  assert.deepEqual(seen, ['s1:1']);
});

test('errors are problem+json: an unknown push route is 404, a missing endpoint 400, an oversized body 413', async () => {
  const { post, url } = await setup();
  const unknown = await post('/push/nope', {});
  assert.equal(unknown.status, 404);
  assert.equal(unknown.headers.get('content-type'), 'application/problem+json');
  assert.equal((await unknown.json()).detail, 'not found');
  assert.equal((await fetch(`${url}/push/subscribe`)).status, 404, 'a GET on a POST route is not a route');
  assert.equal((await post('/push/unsubscribe', {})).status, 400);
  assert.equal((await post('/push/test', { endpoint: 7 })).status, 400);
  assert.equal((await post('/push/test', { endpoint: sub(9).endpoint })).status, 404);
  assert.equal((await post('/push/subscribe', { subscription: 'x'.repeat(32 * 1024) })).status, 413);
});
