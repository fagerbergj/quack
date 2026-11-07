import assert from 'node:assert/strict';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { AddressInfo } from 'node:net';
import { DatabaseSync } from 'node:sqlite';
import { after, test } from 'node:test';
import { openCatalog, type CatalogEvent } from '@quack/catalog';
import { createDistributor, processDriver, sessionDirName, type HostDriver } from '../src/index.ts';
const servers: Array<() => void> = [];
after(() => servers.forEach((close) => close()));

const created = (key: string): CatalogEvent => ({ seq: 1, id: `session:${key}`, at: 0, type: 'session.created', data: { agent: 'a', createdAt: 0 } });
const asked: CatalogEvent = { seq: 2, id: 'ask:1', at: 0, type: 'ask.opened', data: { id: 'a1', type: 'approval', createdAt: 0, toolCall: { tool: 't', args: {} } } };

// A catalog that knows the sessions, with a pending ask on each that waits on a person.
async function catalogOf(keys: string[], waiting: string[] = []) {
  const catalog = openCatalog({ dataDir: await mkdtemp(join(tmpdir(), 'quack-retire-catalog-')) });
  servers.push(() => void catalog.close());
  for (const key of keys) await catalog.publish(key, [created(key), ...(waiting.includes(key) ? [asked] : [])]);
  return catalog;
}

async function start(opts: { running?: string[]; sleeping?: string[]; waiting?: string[] }) {
  const existing = new Set(['idle', 'busy', 'asleep', 'asking']);
  const catalog = await catalogOf([...existing], opts.waiting);
  const driver = {
    running: () => opts.running ?? [], wakes: async () => (opts.sleeping ?? []).map((key) => ({ key, at: 1 })),
    remove: async (key: string) => existing.delete(key), stop: async () => {}, setWake: async () => {}, touch: async () => {},
  } as unknown as HostDriver;
  const server = createDistributor(driver, { catalog }).listen(0);
  await new Promise((r) => server.once('listening', r));
  servers.push(() => server.close());
  const del = (key: string) => fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/${encodeURIComponent(key)}`, { method: 'DELETE' }).then((r) => r.status);
  return { del, catalog, existing };
}

test('deleting an idle session removes its directory and its catalog rows', async () => {
  const { del, catalog, existing } = await start({});
  assert.equal(await del('idle'), 200);
  assert.deepEqual([await catalog.getSession('idle'), existing.has('idle')], [undefined, false]);
  assert.equal(await del('idle'), 404);
});

test('a running, sleeping or person-waiting session is refused and kept', async () => {
  const { del, catalog, existing } = await start({ running: ['busy'], sleeping: ['asleep'], waiting: ['asking'] });
  for (const key of ['busy', 'asleep', 'asking']) assert.equal(await del(key), 409, key);
  assert.equal(existing.size, 4);
  assert.ok(await catalog.getSession('asking'), 'a refused delete keeps the catalog rows');
});

test('a session pinned before retention existed follows its agent current bundle', async () => {
  const root = await mkdtemp(join(tmpdir(), 'quack-ret-'));
  servers.push(() => void rm(root, { recursive: true, force: true }));
  const [agentsDir, dataDir] = [join(root, 'agents'), join(root, 'data')];
  await mkdir(join(agentsDir, 'reviewer'), { recursive: true });
  await writeFile(join(agentsDir, 'reviewer', 'agent.json'), JSON.stringify({ name: 'reviewer', version: 3, retention: { idleDays: 14 } }));
  const pinned = join(dataDir, sessionDirName('old'), 'agent-def');
  await mkdir(pinned, { recursive: true });
  await writeFile(join(pinned, 'agent.json'), JSON.stringify({ name: 'reviewer', version: 2 }));
  await writeFile(join(dataDir, sessionDirName('old'), 'session-key'), 'old');
  await writeFile(join(dataDir, sessionDirName('old'), 'agent'), 'reviewer');
  const [session] = await processDriver({ command: [], dataDir, agentsDir }).sessions();
  assert.deepEqual([session.version, session.retention], [2, { idleDays: 14 }]);
});

test('an invocation racing a delete waits for it and then starts a fresh session', async () => {
  const log: string[] = [];
  let release = () => {};
  const gate = new Promise<void>((r) => (release = r));
  let sawInvoke = () => {};
  const seen = new Promise<void>((r) => (sawInvoke = r));
  const upstream = createServer((_req, res) => res.end('{}')).listen(0);
  servers.push(() => upstream.close());
  await new Promise((r) => upstream.once('listening', r));
  const driver = {
    running: () => [], wakes: async () => [], stop: async () => {}, setWake: async () => {}, touch: async () => {}, credential: () => undefined,
    remove: async () => { await gate; log.push('remove'); return true; },
    ensure: async () => { log.push('ensure'); return `http://127.0.0.1:${(upstream.address() as AddressInfo).port}`; },
  } as unknown as HostDriver;
  const server = createDistributor(driver, {
    catalog: await catalogOf(['old']),
    routes: async (req) => { if (req.method === 'POST') sawInvoke(); return false; },
  }).listen(0);
  await new Promise((r) => server.once('listening', r));
  servers.push(() => server.close());
  const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/old`;
  const deleted = fetch(url, { method: 'DELETE' });
  const invoked = fetch(`${url}/invocations`, { method: 'POST', body: '{}' });
  // setImmediate runs after the request's microtask path has reached its wait on the delete.
  await seen;
  await new Promise((r) => setImmediate(r));
  release();
  assert.equal((await deleted).status, 200);
  assert.equal((await invoked).status, 200);
  assert.deepEqual(log, ['remove', 'ensure']);
});

test('a touch moves the stored last activity forward and never back', async () => {
  const root = await mkdtemp(join(tmpdir(), 'quack-touch-'));
  servers.push(() => void rm(root, { recursive: true, force: true }));
  const dataDir = join(root, 'data');
  const dir = join(dataDir, sessionDirName('job'));
  await mkdir(dir, { recursive: true });
  await writeFile(join(dir, 'session-key'), 'job');
  // A legacy log message: the first touch backfills from it, so an older touch cannot move the value behind it.
  const db = new DatabaseSync(join(dir, 'session.sqlite'));
  db.exec('CREATE TABLE entries (record TEXT)');
  db.prepare('INSERT INTO entries VALUES (?)').run(JSON.stringify({ model: [{ timestamp: Date.parse('2026-11-01T12:00:00Z') }] }));
  db.close();
  const driver = processDriver({ command: [], dataDir, agentsDir: join(root, 'agents') });
  await driver.touch('job', Date.parse('2026-10-31T12:00:00Z'));
  assert.equal((await driver.sessions())[0].lastActivity, '2026-11-01T12:00:00.000Z');
  await driver.touch('job', Date.parse('2026-11-02T12:00:00Z'));
  assert.equal((await driver.sessions())[0].lastActivity, '2026-11-02T12:00:00.000Z');
  await driver.touch('job', Date.parse('2026-11-01T18:00:00Z'));
  assert.equal((await driver.sessions())[0].lastActivity, '2026-11-02T12:00:00.000Z');  // Overlapping touches, newer first: the older one must not land over it.
  await Promise.all([driver.touch('job', Date.parse('2026-11-04T12:00:00Z')), driver.touch('job', Date.parse('2026-11-03T12:00:00Z'))]);
  assert.equal((await driver.sessions())[0].lastActivity, '2026-11-04T12:00:00.000Z');
});
