import assert from 'node:assert/strict';
import { mkdtemp, rm, stat, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { num, obj, str } from '@earendil-works/pi-schema';
import { createCatalog, eventsRoute, fileJournal, sqliteCatalog, type CatalogEvent } from '@quack/catalog';
import { gated, inProcessFetch, pendingApprovals, resolveApproval, setDefaultSessionTitle } from '@quack/sdk';
import { openSession, sessionFacts, startLifecycleSender } from '@quack/session';
import { openNotifications as openPush } from '../src/notifications.ts';

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

let seq = 0;
const ev = (type: string, data: unknown, at = NOW) => ({ seq: ++seq, id: `${type}:${seq}`, at, type, data }) as CatalogEvent;
const approval = (id: string, at = NOW) => ev('ask.opened', { id, type: 'approval', createdAt: at, toolCall: { tool: 'post', args: {} } }, at);
const resolved = (id: string) => ev('ask.resolved', { id, choice: 'approve', by: { kind: 'tailnet' } });
const ended = (outcome: string, run = 1) => ev('run.ended', { run, outcome, failedSteps: [] });

test('a new pending approval sends one plain-words push to each subscription, and a replay sends none', async () => {
  const { push, sent, post } = await setup();
  await post('/push/subscribe', { subscription: sub(1) });
  await post('/push/subscribe', { subscription: sub(2) });
  await push.notify('github:o/r#1', [approval('a1')]);
  await push.notify('github:o/r#1', [approval('a1')]);
  assert.deepEqual(sent.map((s) => s.endpoint).sort(), ['https://push.example/1', 'https://push.example/2']);
  assert.deepEqual(sent[0]!.payload, { title: 'Approval waiting: Review #2095', body: '', url: '#/s/github%3Ao%2Fr%231' });
});

test('what was sent survives a restart, and resolved or stale events send nothing', async () => {
  const { dir, push, sent, post } = await setup();
  await post('/push/subscribe', { subscription: sub(1) });
  await push.notify('s', [approval('a1')]);
  push.close();
  const again = openPush({ dataDir: dir, now: () => NOW, send: async (_s, p) => void sent.push({ endpoint: '', payload: JSON.parse(p) }) });
  await again.notify('s', [approval('a1'), resolved('a2'), approval('a3', NOW - 7 * 3600_000), ev('judge.cleared', {}) as CatalogEvent]);
  again.close();
  assert.equal(sent.length, 1);
});

test('questions and failed runs each push once; a later failed run pushes again', async () => {
  const { push, sent, post } = await setup();
  await post('/push/subscribe', { subscription: sub(1) });
  const question = ev('ask.opened', { id: 'q1', type: 'question', createdAt: NOW, prompt: { question: 'Ship  it?\nnow', options: ['y'], freeText: false } });
  await push.notify('s', [question, ended('failed', 1), ended('failed', 1), ended('failed', 2)]);
  assert.deepEqual(sent.map((s) => [s.payload.title, s.payload.body]), [
    ['Question waiting: Review #2095', 'Ship it? now'], ['Session failed: Review #2095', ''], ['Session failed: Review #2095', ''],
  ]);
});

test('an approval shows its summary', async () => {
  const { push, sent, post } = await setup();
  await post('/push/subscribe', { subscription: sub(1) });
  await push.notify('s', [ev('ask.opened', { id: 'a9', type: 'approval', createdAt: NOW, toolCall: { tool: 'post', args: { summary: 'Post the  review' } } })]);
  assert.equal(sent[0]!.payload.body, 'Post the review');
});

test("a run's end pushes the newest version of each kind as a report, once per version; a failed run's does not", async () => {
  const stored = [{ kind: 'cards', version: 1 }, { kind: 'cards', version: 2 }, { kind: 'review', version: 1 }];
  const dir = await mkdtemp(join(tmpdir(), 'quack-push-'));
  dirs.push(dir);
  const sent: string[] = [];
  const push = openPush({ dataDir: dir, now: () => NOW, artifacts: async () => stored, send: async (_s, p) => void sent.push(JSON.parse(p).title) });
  const server = createServer((req, res) => void push.routes(req, res));
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  await fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/push/subscribe`, { method: 'POST', body: JSON.stringify({ subscription: sub(1) }) });
  server.close();
  await push.notify('s', [ev('artifact.sealed', { kind: 'cards', version: 2, hash: 'h', content: 'c' })]);
  assert.deepEqual(sent, [], 'a seal alone is not a report');
  await push.notify('s', [ended('failed', 1)]);
  assert.deepEqual(sent, ['Session failed']);
  await push.notify('s', [ended('succeeded', 2)]);
  await push.notify('s', [ended('succeeded', 3)]);
  stored.push({ kind: 'cards', version: 3 });
  await push.notify('s', [ended('succeeded', 4)]);
  assert.deepEqual(sent, ['Session failed', 'Report ready', 'Report ready', 'Report ready']);
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

// End to end: a session names itself and asks the way the contracts gate does, its lifecycle stream is applied to a real
// catalog, and each applied batch is pushed with the catalog's title.
async function namingSession() {
  const dir = await mkdtemp(join(tmpdir(), 'quack-push-title-'));
  dirs.push(dir);
  const key = 'contracts:push';
  await writeFile(join(dir, 'session-key'), key);
  await writeFile(join(dir, 'created'), String(NOW));
  const propose = gated(defineTool({ name: 'propose_contract', description: 'put a version up', parameters: obj({ version: num() }, { open: true }), execute: async () => ({ output: [{ type: 'text', text: 'approved' }] }) }));
  const submit = defineTool({
    name: 'submit_contract', description: 'seal and propose', parameters: obj({ title: str(), version: num() }, { open: true }), replay: 'safe',
    async execute(args, api, c) {
      const { title, version } = args as { title: string; version: number };
      await setDefaultSessionTitle(api, title, c);
      return propose.execute({ version } as never, api, c);
    },
  });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  const proposeV = (title: string, version: number) => fauxAssistantMessage(fauxToolCall('submit_contract', { title, version }), { stopReason: 'toolUse' });
  faux.setResponses([proposeV('research.report artifact', 1), proposeV('research.report artifact, parsed', 2), fauxAssistantMessage('Done.')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir, models, now: () => new Date(NOW), agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'contracts', tools: [submit] })] } });
  const catalog = createCatalog({ store: sqliteCatalog(join(dir, 'catalog.sqlite'), { currency: 'USD' }), journal: fileJournal(join(dir, 'journal')), now: () => NOW, log: () => {} });
  const sent: string[] = [];
  const push = openPush({ dataDir: dir, now: () => NOW, title: async (k) => (await catalog.getSession(k))?.title, send: async (_s, p) => void sent.push(JSON.parse(p).title) });
  const route = eventsRoute(catalog, (k, events) => push.notify(k, events));
  const fetcher = inProcessFetch(async (req, res) => void ((await route(req, res, key)) || res.writeHead(404).end()));
  const sender = await startLifecycleSender(session, { url: 'http://d', token: 't', key, facts: () => sessionFacts(dir), fetch: fetcher, setTimer: () => () => {}, now: () => NOW });
  after(async () => { await sender.stop(); await session.close(); await catalog.close(); push.close(); });
  const server = createServer((req, res) => void push.routes(req, res));
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  await fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/push/subscribe`, { method: 'POST', body: JSON.stringify({ subscription: sub(1) }) });
  server.close();
  // Flushes until `done` holds; the sender's own watches race these, which only ever sends sooner.
  const until = async (done: () => boolean | Promise<boolean>) => { for (let i = 0; i < 200 && !(await done()); i++) { await sender.flush(); await new Promise((r) => setImmediate(r)); } };
  const pending = async () => (await pendingApprovals(session.root, ctx)).map((a) => a.id);
  return { key, session, catalog, sent, until, pending };
}

test("a contract's approval push names the contract; after a person's rename, a later version's push and the list keep the person's title", async () => {
  const s = await namingSession();
  const run = s.session.submit('Design the JSON Schema contract (kind json-schema) for the research.report artifact, in its parsed form', undefined, true);
  await s.until(() => s.sent.length > 0);
  assert.deepEqual(s.sent, ['Approval waiting: research.report artifact']);
  await s.catalog.rename(s.key, 'Report contract');
  const [first] = await s.pending();
  await resolveApproval(s.session.root, first!, { decision: 'reject', reason: 'parse it first', by: { kind: 'person', name: 'jf' } }, ctx);
  await s.until(() => s.sent.length > 1);
  assert.deepEqual(s.sent, ['Approval waiting: research.report artifact', 'Approval waiting: Report contract']);
  const [second] = await s.pending();
  await resolveApproval(s.session.root, second!, { decision: 'approve', by: { kind: 'person', name: 'jf' } }, ctx);
  await run;
  const listed = async () => (await s.catalog.listSessions({})).rows.map((r) => r.title);
  await s.until(async () => (await s.catalog.getSession(s.key))?.state === 'idle');
  assert.deepEqual(await listed(), ['Report contract']);
  await s.catalog.rename(s.key, null);
  assert.deepEqual(await listed(), ['research.report artifact, parsed'], 'the newest set title is the default under the rename');
});
