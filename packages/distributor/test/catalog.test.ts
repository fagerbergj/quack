import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { mkdir, mkdtemp, readdir, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { eventsRoute, openCatalog } from '@quack/catalog';
import { inProcessFetch } from '@quack/sdk';
import { LifecycleStream, openSession, sessionFacts, startLifecycleSender } from '@quack/session';
import { backfillCatalog } from '../src/catalog-sync.ts';
import { createDistributor, processDriver, sessionDirName, type HostDriver } from '../src/index.ts';
import { PERSON, PROXY_SECRET } from './fixtures/proxy.ts';

const cleanups: Array<() => Promise<unknown> | void> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

async function temp() {
  const dir = await mkdtemp(join(tmpdir(), 'quack-dist-catalog-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  return dir;
}

// A session from before the catalog: a log with a finished turn, and no lifecycle stream in it.
async function legacySession(dataDir: string, key: string) {
  const dir = join(dataDir, sessionDirName(key));
  await mkdir(dir, { recursive: true });
  await writeFile(join(dir, 'session-key'), key);
  await writeFile(join(dir, 'agent'), 'quack');
  await writeFile(join(dir, 'created'), String(Date.parse('2026-10-01T00:00:00Z')));
  await writeFile(join(dir, 'activity'), String(Date.parse('2026-10-02T00:00:00Z')));
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage('Answered.')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const open = () => openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id } } });
  const session = await open();
  await session.submit('Check the lineup for matchday 2', undefined, true);
  await session.close();
  return { dir, open };
}

test('the first boot backfills existing sessions from their logs, seeds each stream, and runs once', async () => {
  const dataDir = await temp();
  const old = await legacySession(dataDir, 'sleeper:platinum:2:lineup');
  const catalog = openCatalog({ dataDir });
  cleanups.push(() => catalog.close());
  assert.equal(await backfillCatalog(catalog, dataDir, () => undefined), 1);
  const row = await catalog.getSession('sleeper:platinum:2:lineup');
  assert.deepEqual([row?.agent, row?.title, row?.state, row?.createdAt, row?.lastActivity, row?.lastRun?.outcome],
    ['quack', 'Check the lineup for matchday 2', 'stopped', Date.parse('2026-10-01T00:00:00Z'), Date.parse('2026-10-02T00:00:00Z'), 'succeeded']);
  assert.equal(await backfillCatalog(catalog, dataDir, () => undefined), undefined, 'a later boot skips it');
  assert.ok(existsSync(join(dataDir, 'catalog-journal')) || !(await readdir(dataDir)).includes('catalog-journal'), 'the journal, when written, is the data dir\'s own');

  // The session, started on the new runtime, continues the seeded numbering: nothing it sends is new to the catalog.
  const session = await old.open();
  const posted: number[] = [];
  const route = eventsRoute(catalog);
  const served = inProcessFetch(async (req, res) => void ((await route(req, res, 'sleeper:platinum:2:lineup')) || res.writeHead(404).end()));
  const sender = await startLifecycleSender(session, {
    url: 'http://d', token: 't', key: 'sleeper:platinum:2:lineup', facts: () => sessionFacts(old.dir), setTimer: () => () => {},
    fetch: async (url, init) => { posted.push(...JSON.parse(String(init?.body)).events.map((e: { seq: number }) => e.seq)); return served(url, init); },
  });
  const seeded = (await session.harness.snapshot(LifecycleStream, session.root.id, ctx))!;
  await sender.flush();
  await sender.stop();
  await session.close();
  assert.ok(seeded.events.length > 0, 'the backfill recorded the stream in the log');
  assert.equal(await catalog.watermark('sleeper:platinum:2:lineup'), seeded.events.length, 'the catalog already held every seq the session re-sent');
  assert.deepEqual((await catalog.getSession('sleeper:platinum:2:lineup'))?.title, row?.title);
  assert.ok(posted.every((seq) => seq <= seeded.events.length));
});

// A distributor over a real data dir whose sessions are directories only; `exited` plays a process ending.
async function distributor() {
  const dataDir = await temp();
  for (const key of ['idle', 'other']) {
    await mkdir(join(dataDir, sessionDirName(key)), { recursive: true });
    await writeFile(join(dataDir, sessionDirName(key), 'session-key'), key);
    await writeFile(join(dataDir, sessionDirName(key), 'agent'), 'quack');
  }
  const catalog = openCatalog({ dataDir });
  cleanups.push(() => catalog.close());
  for (const key of ['idle', 'other']) await catalog.publish(key, [{ seq: 1, id: `session:${key}`, at: 0, type: 'session.created', data: { agent: 'quack', createdAt: 0 } }]);
  const real = processDriver({ command: [], dataDir, onExit: (key) => void catalog.stopped(key) });
  const upstream = createServer((_req, res) => res.end(JSON.stringify({ status: 'Healthy' }))).listen(0);
  cleanups.push(() => void upstream.close());
  const driver: HostDriver = { ...real, running: () => [], ensure: async () => `http://127.0.0.1:${(upstream.address() as AddressInfo).port}`, touch: async () => {}, setWake: async () => {} };
  const tokens = new Map([['tok-idle', 'idle']]);
  const sessions = { sessionOf: (t: string) => tokens.get(t), crossSession: async () => false, events: eventsRoute(catalog) };
  const server = createDistributor(driver, { proxySecret: PROXY_SECRET, sessions, catalog, setTimer: () => () => {}, retire: { waiting: async () => false, forget: async () => {} } });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  await new Promise<void>((r) => server.sessions.listen(0, '127.0.0.1', r));
  cleanups.push(() => void server.close());
  const base = (s: { address(): unknown }) => `http://127.0.0.1:${(s.address() as AddressInfo).port}`;
  return { catalog, base: base(server), sessionBase: base(server.sessions) };
}

const batch = (seq: number) => JSON.stringify({ events: [{ seq, id: 'title', at: '2026-11-06T09:00:00Z', type: 'title.set', data: { title: 'Reported' } }] });

test('the channel answers only on the session listener, for the credential\'s own key', async () => {
  const { catalog, base, sessionBase } = await distributor();
  const post = (url: string, key: string, token?: string) => fetch(`${url}/sessions/${key}/events`, { method: 'POST', headers: { 'content-type': 'application/json', ...(token && { authorization: `Bearer ${token}` }) }, body: batch(2) });
  assert.equal((await post(sessionBase, 'idle')).status, 401);
  assert.equal((await post(sessionBase, 'idle', 'stolen')).status, 401);
  assert.equal((await post(sessionBase, 'other', 'tok-idle')).status, 404);
  const ok = await post(sessionBase, 'idle', 'tok-idle');
  assert.deepEqual([ok.status, await ok.json()], [200, { acked: 2 }]);
  assert.equal((await catalog.getSession('idle'))?.title, 'Reported');
  await post(base, 'other', 'tok-idle');
  assert.equal((await catalog.getSession('other'))?.title, undefined, 'the public listener never applies events');
});

test('a person\'s rename, archive and delete reach the catalog; a process exit records stopped', async () => {
  const { catalog, base } = await distributor();
  const call = (method: string, key: string, body?: unknown) => fetch(`${base}/sessions/${key}`, { method, headers: { ...PERSON, 'content-type': 'application/json' }, ...(body !== undefined && { body: JSON.stringify(body) }) });
  assert.equal((await call('PATCH', 'idle', { title: 'Mine' })).status, 200);
  assert.equal((await catalog.getSession('idle'))?.title, 'Mine');
  assert.equal((await call('PATCH', 'idle', { archived: true })).status, 200);
  assert.equal((await catalog.getSession('idle'))?.state, 'archived');
  assert.equal((await call('PATCH', 'idle', { archived: false })).status, 200);
  assert.equal((await call('DELETE', 'idle')).status, 200);
  assert.equal(await catalog.getSession('idle'), undefined);
  await catalog.publish('other', [{ seq: 2, id: 'run:1:started', at: 1, type: 'run.started', data: { run: 1 } }]);
  assert.equal((await catalog.getSession('other'))?.state, 'running');
  assert.deepEqual(await catalog.stopped('other'), { applied: true, state: 'stopped' });
});

test('the root catalog section names the catalog store; the loader checks it and refuses an index extension', async () => {
  const { catalogStore } = await import('@quack/catalog');
  const { launchEnv, parseDeployment } = await import('@quack/session');
  const stores = { cat: { kind: 'postgres' as const, urlEnv: 'CAT_URL' } };
  assert.deepEqual(catalogStore({ stores, catalog: { store: 'cat' } }, { CAT_URL: 'postgres://x' }), { kind: 'postgres', url: 'postgres://x' }, 'a named store');
  assert.deepEqual(catalogStore({}, {}), { kind: 'sqlite' }, 'no section is sqlite in the data dir');
  assert.deepEqual(catalogStore({ catalog: {} }, {}), { kind: 'sqlite' }, 'a section without a store too');
  assert.deepEqual(catalogStore({ stores: { catalog: stores.cat } }, { CAT_URL: 'postgres://x' }), { kind: 'sqlite' }, 'a store merely named catalog is not the catalog\'s');
  assert.throws(() => catalogStore({ stores, catalog: { store: 'nope' } }, {}), /store nope is missing/);
  assert.throws(() => catalogStore({ extensions: { index: { store: 'cat' } } }, {}), /extensions\.index is refused.*catalog\.store/);
  const base = { providers: { local: { baseUrl: 'http://x', models: { m: { contextWindow: 1000 } } } }, defaultModel: 'local/m', stores };
  assert.doesNotThrow(() => parseDeployment({ ...base, catalog: { store: 'cat' } }));
  assert.throws(() => parseDeployment({ ...base, catalog: { store: 'nope' } }), /catalog\.store nope is not in stores/);
  assert.throws(() => parseDeployment({ ...base, catalog: { store: 'cat', path: 'x' } }), /catalog\.path is not a catalog key/);
  assert.throws(() => parseDeployment({ ...base, extensions: { index: {} } }), /extensions\.index is refused: the catalog is core, so name its store at catalog\.store/);
  assert.deepEqual(launchEnv({ QUACK_INDEX_URL: 'http://d', QUACK_INDEX_TOKEN: 't' }), { url: 'http://d', token: 't' });
  assert.equal(launchEnv({ QUACK_INDEX_URL: 'http://d' }), undefined);
});
