import assert from 'node:assert/strict';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { artifactsExtension, attachVerdict, gated, pendingApprovals, resolveApproval, seal, type ArtifactKind } from '@quack/sdk';
import { createSessionServer, openSession } from '@quack/session';
import { defineWorkflow, outcomeOf, sessionDecisions, WorkflowDoc, workflowSessionExtensions } from '@quack/workflows';
import pg from 'pg';
import { DatabaseSync } from 'node:sqlite';
import { existsSync } from 'node:fs';
import { createStore, openStore, indexRoutes, postgresBackend, rebuild, sqliteBackend, startPublisher, type IndexStore } from '../src/index.ts';

const url = process.env.QUACK_TEST_DATABASE_URL;
const ctx = BACKGROUND_CONTEXT;
const TOKEN = 'test-token';
const cleanups: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

type Fresh = { store: IndexStore };

// Each store gets its own schema so tests never see each other's rows.
async function freshPostgres(): Promise<Fresh> {
  const schema = `t_${Math.random().toString(36).slice(2, 10)}`;
  const admin = new pg.Pool({ connectionString: url, max: 1 });
  await admin.query(`create schema ${schema}`);
  const pool = new pg.Pool({ connectionString: url, options: `-c search_path=${schema}` });
  cleanups.push(async () => { await pool.end(); await admin.query(`drop schema ${schema} cascade`); await admin.end(); });
  const store = createStore(postgresBackend(pool));
  await store.ensure();
  return { store };
}

async function freshSqlite(): Promise<Fresh> {
  const dir = await mkdtemp(join(tmpdir(), 'quack-index-db-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  const store = createStore(sqliteBackend(join(dir, 'index.sqlite')));
  await store.ensure();
  return { store };
}

const review: ArtifactKind = { kind: 'demo.review', path: 'review.md' };
const submitReview = defineTool({
  name: 'submit_review', description: 'seal', parameters: Type.Object({}), replay: 'safe',
  execute: async (_a, api, c) => {
    const sealed = await seal(api, review, c);
    await attachVerdict(api, sealed.kind, sealed.version, { passed: true }, c);
    return { output: [{ type: 'text', text: 'sealed' }] };
  },
});
const post = gated(defineTool({ name: 'post', description: 'post', parameters: Type.Object({}), execute: async () => ({ output: [{ type: 'text', text: 'posted' }] }) }));

async function openDemoSession(dir: string) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([
    fauxAssistantMessage(fauxToolCall('write', { path: 'review.md', content: '# Review\nfine\n' }), { stopReason: 'toolUse' }),
    fauxAssistantMessage(fauxToolCall('submit_review', {}), { stopReason: 'toolUse' }),
    fauxAssistantMessage(fauxToolCall('post', {}), { stopReason: 'toolUse' }),
    fauxAssistantMessage('Done.'),
  ]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  return openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, artifactsExtension([review]), defineExtension({ name: 'demo', tools: [submitReview, post] })] } });
}

async function until<T>(probe: () => Promise<T | undefined>, ms = 15_000): Promise<T> {
  const deadline = Date.now() + ms;
  for (;;) {
    const v = await probe();
    if (v !== undefined) return v;
    if (Date.now() > deadline) throw new Error('condition not reached');
    await new Promise((r) => setTimeout(r, 20));
  }
}

const listen = (server: Server) => new Promise<number>((r) => server.listen(0, () => r((server.address() as AddressInfo).port)));

const backends = [
  { name: 'sqlite', fresh: freshSqlite, skip: false },
  { name: 'postgres', fresh: freshPostgres, skip: !url && 'QUACK_TEST_DATABASE_URL is not set' },
];

for (const { name, fresh: freshStore, skip } of backends) test(`${name}: facts survive the index being down, land once, and replaying the log rebuilds the same rows`, { skip }, async () => {
  const { store } = await freshStore();
  const routes = indexRoutes(store, TOKEN);
  let down = true;
  const index = createServer(async (req, res) => {
    if (down) return void res.writeHead(503).end('{}');
    if (!(await routes(req, res))) res.writeHead(404).end('{}');
  });
  const base = `http://127.0.0.1:${await listen(index)}`;
  cleanups.push(() => new Promise((r) => index.close(r)));
  const get = async (path: string) => (await fetch(base + path)).json() as Promise<Record<string, Array<Record<string, unknown>>>>;

  const dataDir = await mkdtemp(join(tmpdir(), 'quack-index-'));
  cleanups.push(() => rm(dataDir, { recursive: true, force: true }));
  const dir = join(dataDir, 'abc');
  await mkdir(dir);
  await writeFile(join(dir, 'session-key'), 'chat:1');
  await writeFile(join(dir, 'agent'), 'reviewer');
  await mkdir(join(dir, 'agent-def'));
  await writeFile(join(dir, 'agent-def', 'agent.json'), JSON.stringify({ name: 'reviewer', version: 4 }));
  const session = await openDemoSession(dir);
  // retryMs is far past the test, so only explicit flushes retry.
  const publisher = await startPublisher(session, { url: base, token: TOKEN, meta: { key: 'chat:1', agent: 'reviewer', version: 4 }, retryMs: 3_600_000 });

  await session.submit('Review it.', 'r1');
  const approval = await until(async () => (await pendingApprovals(session.root, ctx))[0]);
  await publisher.flush();
  assert.equal((await store.facts()).length, 0, 'nothing reaches a down index');

  down = false;
  await publisher.flush();
  assert.deepEqual((await get('/index/approvals?state=pending')).approvals.map((a) => [a.session, a.id, a.tool, a.state]), [['chat:1', approval.id, 'post', 'pending']]);
  assert.deepEqual((await get('/index/artifacts')).artifacts.map((a) => [a.session, a.kind, a.version, a.verdict]), [['chat:1', 'demo.review', 1, { passed: true }]]);
  const [row] = (await get('/index/sessions')).sessions;
  assert.deepEqual([row.key, row.agent, row.version, row.status], ['chat:1', 'reviewer', 4, 'running']);

  assert.equal(await resolveApproval(session.root, approval.id, { decision: 'approve', by: 'jason' }, ctx), true);
  const idle = await until(async () => (await publisher.flush(), (await get('/index/sessions')).sessions[0].status === 'idle' ? true : undefined));
  assert.ok(idle);
  assert.deepEqual((await get('/index/approvals?state=pending')).approvals, []);
  assert.deepEqual((await get('/index/approvals?state=resolved')).approvals.map((a) => [a.decision, a.by]), [['approve', 'jason']]);

  const rows = async (s: IndexStore) => (await s.facts()).sort((a, b) => a.session.localeCompare(b.session) || a.id.localeCompare(b.id));
  const live = await rows(store);
  assert.equal(new Set(live.map((r) => r.id)).size, live.length);
  const body = JSON.stringify({ session: 'chat:1', facts: [{ id: 'session', kind: 'session', rev: 1, at: 0, data: { agent: 'other', version: 9 } }] });
  const headers = { authorization: `Bearer ${TOKEN}`, 'content-type': 'application/json' };
  for (let i = 0; i < 2; i++) assert.equal((await fetch(`${base}/index/facts`, { method: 'POST', headers, body })).status, 200);
  assert.deepEqual(await rows(store), live, 'a resent or stale fact changes nothing');
  assert.equal((await fetch(`${base}/index/facts`, { method: 'POST', body })).status, 401);

  await publisher.stop();
  await session.close();
  const fresh = await freshStore();
  assert.equal(await rebuild(fresh.store, dataDir), 1);
  assert.deepEqual(await rows(fresh.store), live, 'replaying the log reproduces the index');
  await rebuild(fresh.store, dataDir);
  assert.deepEqual(await rows(fresh.store), live, 'replaying twice yields the same rows');
});

test('openStore defaults to the sqlite file in the data dir', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-index-open-'));
  const { store, close } = openStore({ QUACK_DATA_DIR: dir });
  try {
    await store.ensure();
    assert.ok(existsSync(join(dir, 'index.sqlite')));
    await store.upsert('chat:1', [{ id: 'session', kind: 'session', rev: 1, at: 0, data: { agent: 'reviewer', version: 4 } }]);
    assert.equal((await store.facts()).length, 1);
  } finally {
    await close();
    await rm(dir, { recursive: true, force: true });
  }
});

test('a failed sqlite migration rolls back, so ensure recovers once the cause is gone', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-index-mig-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  const path = join(dir, 'index.sqlite');
  const blocker = new DatabaseSync(path);
  blocker.exec('create table index_facts (x)');
  const backend = sqliteBackend(path);
  const store = createStore(backend);
  cleanups.push(async () => backend.close());
  await assert.rejects(store.ensure(), /already exists/);
  blocker.exec('drop table index_facts');
  blocker.close();
  await store.ensure();
  await store.upsert('chat:1', [{ id: 'session', kind: 'session', rev: 1, at: 0, data: {} }]);
  assert.equal((await store.facts()).length, 1);
});

const ask = defineWorkflow({
  name: 'ask',
  steps: [
    { name: 'type', kind: 'decision', point: () => ({ question: 'What kind of document is this?', options: ['meeting', 'idea'], evidence: 'Agenda: standup' }), resolvers: [{ id: 'unsure', resolve: async () => undefined }] },
    { name: 'file', kind: 'code', run: (c) => `filed as ${(c.prior.type as { choice: string }).choice}` },
  ],
});

for (const { name, fresh: freshStore, skip } of backends) test(`${name}: a decision a workflow escalates is indexed, answered over the session route, and the workflow resumes`, { skip }, async () => {
  const { store } = await freshStore();
  const routes = indexRoutes(store, TOKEN);
  const index = createServer(async (req, res) => { if (!(await routes(req, res))) res.writeHead(404).end(); });
  const indexBase = `http://127.0.0.1:${await listen(index)}`;
  cleanups.push(() => new Promise((r) => index.close(r)));

  const dir = await mkdtemp(join(tmpdir(), 'quack-decide-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: workflowSessionExtensions({ workflow: 'ask' }, { ask }) } });
  const publisher = await startPublisher(session, { url: indexBase, token: TOKEN, meta: { key: 'chat:9', agent: 'ingest', version: 1 }, pollMs: 20 });
  const server = createSessionServer(session, sessionDecisions(session)).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const answer = (step: string, body: unknown) => fetch(`${base}/decisions/${step}`, { method: 'POST', body: JSON.stringify(body) });
  const listed = async (state: string) => ((await (await fetch(`${indexBase}/index/decisions?state=${state}`)).json()) as { decisions: Array<Record<string, unknown>> }).decisions;

  await fetch(`${base}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'scan' }) });
  const [open] = await until(async () => { const d = await listed('pending'); return d.length ? d : undefined; });
  assert.deepEqual([open.session, open.step, open.question, open.options, open.evidence], ['chat:9', 'type', 'What kind of document is this?', ['meeting', 'idea'], 'Agenda: standup']);
  assert.deepEqual(await (await fetch(`${base}/decisions`)).json(), { decisions: [{ step: 'type', question: open.question, options: open.options, evidence: open.evidence, askedAt: open.at }] });

  assert.equal((await answer('type', { choice: 'poem', by: 'jf' })).status, 400, 'only a listed option answers');
  assert.equal((await answer('file', { choice: 'meeting', by: 'jf' })).status, 404, 'a step that is not asking');
  assert.equal((await answer('type', { choice: 'meeting', by: 'jf' })).status, 200);
  assert.equal((await answer('type', { choice: 'idea', by: 'jf' })).status, 409, 'the first answer stands');

  await until(async () => { return (await listed('answered')).length ? true : undefined; });
  assert.deepEqual(await listed('pending'), []);
  assert.deepEqual((await listed('answered')).map((d) => [d.choice, d.by]), [['meeting', 'jf']]);
  const task = String((await session.harness.snapshot(WorkflowDoc, session.root.id, ctx))!.task);
  const outcome = await outcomeOf(session.harness, task);
  assert.equal(outcome.status === 'completed' && (outcome.result as { file: string }).file, 'filed as meeting');

  await publisher.stop();
  await new Promise((r) => server.close(r));
  await session.close();
});
