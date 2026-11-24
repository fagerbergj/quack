import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, mock, test } from 'node:test';
import { openCatalog } from '@quack/catalog';
import { parseDeployment, readModelChoice } from '@quack/session';
import { modelRoutes, sessionModels } from '../src/models.ts';
import { createDistributor, digest, processDriver, sessionDirName, sessionHost, type HostDriver, type Launch } from '../src/index.ts';
import { PERSON, PROXY_SECRET } from './fixtures/proxy.ts';

const cleanups: Array<() => Promise<unknown> | void> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

async function temp() {
  const dir = await mkdtemp(join(tmpdir(), 'quack-models-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  return dir;
}

// llm-swap lists `big` with its context; `hand` is only hand-written, and `clef` is a decision model, never a session's.
const deployment = parseDeployment({
  providers: {
    local: { baseUrl: 'http://swap/v1', models: { hand: { contextWindow: 4096 } } },
    clef: { baseUrl: 'http://clef/v1', api: 'systemone', models: { clef: { contextWindow: 8192 } } },
  },
});
const listing = (async () => ({ ok: true, json: async () => ({ data: [{ id: 'big', meta: { n_ctx: 262144 }, architecture: { input_modalities: ['text', 'image'] } }, { id: 'no-ctx' }] }) })) as unknown as typeof fetch;
const down = (async () => { throw new Error('refused'); }) as unknown as typeof fetch;

test('the model list is what each provider\'s /models states plus the hand-written ones, and falls back to those when discovery fails', async () => {
  const dataDir = await temp();
  assert.deepEqual(await sessionModels({ dataDir, deployment, fetch: listing }).list(), {
    models: [{ name: 'local/big', contextWindow: 262144, images: true }, { name: 'local/hand', contextWindow: 4096 }],
    defaultModel: 'local/big',
  }, 'with no default anywhere the first available is the default');
  const warn = mock.method(console, 'error', () => {});
  assert.deepEqual(await sessionModels({ dataDir, deployment, fetch: down }).list(), { models: [{ name: 'local/hand', contextWindow: 4096 }], defaultModel: 'local/hand' });
  warn.mock.restore();
});

test('the Settings default is checked, persists in the data dir across restarts, and outranks the deployment\'s', async () => {
  const dataDir = await temp();
  const withDefault = parseDeployment({ ...deployment, defaultModel: 'local/hand' });
  const models = sessionModels({ dataDir, deployment: withDefault, fetch: listing });
  assert.equal((await models.list()).defaultModel, 'local/hand', 'the deployment default is the fallback');
  assert.match(String(await models.setDefault('local/no-ctx')), /model local\/no-ctx is not available; available: local\/big, local\/hand/);
  assert.match(String(await models.setDefault('clef/clef')), /not available/, 'a decision model is no session model');
  assert.equal(await models.setDefault('local/big'), undefined);
  const restarted = sessionModels({ dataDir, deployment: withDefault, fetch: listing });
  assert.deepEqual(await restarted.settings(), { defaultModel: 'local/big' });
  assert.equal((await restarted.list()).defaultModel, 'local/big');
  await restarted.setDefault(null);
  assert.deepEqual(await restarted.settings(), {});
});

// A distributor over a real data dir with one stopped session, `idle`, and the model routes mounted as main mounts them.
async function distributor(driver?: (real: HostDriver) => HostDriver) {
  const dataDir = await temp();
  await mkdir(join(dataDir, sessionDirName('idle')), { recursive: true });
  await writeFile(join(dataDir, sessionDirName('idle'), 'session-key'), 'idle');
  await writeFile(join(dataDir, sessionDirName('idle'), 'agent'), 'quack');
  const catalog = openCatalog({ dataDir });
  cleanups.push(() => catalog.close());
  await catalog.publish('idle', [{ seq: 1, id: 'session:idle', at: 0, type: 'session.created', data: { agent: 'quack', createdAt: 0 } }]);
  const models = sessionModels({ dataDir, deployment, fetch: listing });
  const real = processDriver({ command: [], dataDir });
  const server = createDistributor(driver?.(real) ?? real, { proxySecret: PROXY_SECRET, catalog, models, routes: modelRoutes(models), setTimer: () => () => {} });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  cleanups.push(() => void server.close());
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const call = (method: string, path: string, body?: unknown) => fetch(`${base}${path}`, { method, headers: { ...PERSON, 'content-type': 'application/json' }, ...(body !== undefined && { body: JSON.stringify(body) }) });
  return { dataDir, call, server };
}

test('GET /models lists the available models and PATCH /settings sets the default, refusing one that is not available', async () => {
  const { call } = await distributor();
  assert.deepEqual(await (await call('GET', '/models')).json(), { models: [{ name: 'local/big', contextWindow: 262144, images: true }, { name: 'local/hand', contextWindow: 4096 }], defaultModel: 'local/big' });
  const refused = await call('PATCH', '/settings', { defaultModel: 'local/nope' });
  assert.equal(refused.status, 400);
  assert.match((await refused.json()).detail, /local\/nope is not available/);
  assert.equal((await call('PATCH', '/settings', { defaultModel: 7 })).status, 400);
  assert.deepEqual(await (await call('PATCH', '/settings', { defaultModel: 'local/hand' })).json(), { defaultModel: 'local/hand' });
  assert.deepEqual(await (await call('GET', '/settings')).json(), { defaultModel: 'local/hand' });
  assert.equal((await (await call('GET', '/models')).json()).defaultModel, 'local/hand');
});

test('a person sets a session\'s model through the Sessions API; an unavailable one is refused and nothing is written', async () => {
  const { dataDir, call } = await distributor();
  const refused = await call('PATCH', '/sessions/idle', { model: 'local/no-ctx', title: 'Kept out' });
  assert.equal(refused.status, 400);
  assert.match((await refused.json()).detail, /model local\/no-ctx is not available; available: local\/big, local\/hand/);
  assert.equal((await (await call('GET', '/sessions/idle')).json()).title, undefined, 'the title in the refused patch is not written');
  assert.equal((await call('PATCH', '/sessions/idle', { model: 'no-slash' })).status, 400);
  const set = await call('PATCH', '/sessions/idle', { model: 'local/hand' });
  assert.equal((await set.json()).model, 'local/hand');
  assert.equal((await (await call('GET', '/sessions/idle')).json()).model, 'local/hand');
  assert.deepEqual(readModelChoice(join(dataDir, sessionDirName('idle'))), { session: 'local/hand' }, 'the pick is in the session dir, where its next start reads it');
  await call('PATCH', '/sessions/idle', { model: null });
  assert.equal((await (await call('GET', '/sessions/idle')).json()).model, undefined);
});

test('a pick restarts an idle process at once and a busy one only once its run ends, so running turns keep their model', async () => {
  let status = 'HealthyBusy';
  const upstream = createServer((_req, res) => res.end(JSON.stringify({ status })));
  await new Promise<void>((r) => upstream.listen(0, '127.0.0.1', r));
  cleanups.push(() => void upstream.close());
  const stops: string[] = [];
  const live = new Set(['idle']);
  const { call, server } = await distributor((real) => ({
    ...real, running: () => [...live], setWake: async () => {},
    ensure: async () => `http://127.0.0.1:${(upstream.address() as AddressInfo).port}`,
    stop: async (key) => void (stops.push(key), live.delete(key)),
  }));
  assert.equal((await call('PATCH', '/sessions/idle', { model: 'local/hand' })).status, 200);
  assert.deepEqual(stops, [], 'the busy run keeps its process and model');
  await server.sweep();
  assert.deepEqual(stops, [], 'still busy');
  status = 'Healthy';
  await server.sweep();
  assert.deepEqual(stops, ['idle'], 'stopped between runs, so the next run starts on the pick');
  live.add('idle');
  await call('PATCH', '/sessions/idle', { model: 'local/big' });
  assert.deepEqual(stops, ['idle', 'idle'], 'an idle process restarts at once');
});

test('the pick is a session override beside the pin: the pinned snapshot is unchanged and each start gets the Settings default', async () => {
  const [dataDir, agentsDir] = [await temp(), await temp()];
  await mkdir(join(agentsDir, 'quack'));
  await writeFile(join(agentsDir, 'quack', 'agent.json'), JSON.stringify({ name: 'quack', version: 1, extensions: {} }));
  let settingsDefault: string | undefined = 'local/hand';
  const seen: Array<ReturnType<typeof readModelChoice>> = [];
  const launch: Launch = async ({ dir }) => (seen.push(readModelChoice(dir)), { url: 'http://session.invalid', token: 't', stop: async () => {}, onExit: () => {} });
  const driver = sessionHost({ dataDir, agentsDir, defaultModel: async () => settingsDefault }, launch);
  await driver.ensure('s', 'quack');
  const snapshot = join(dataDir, sessionDirName('s'), 'agent-def');
  const pinned = await digest(snapshot);
  await sessionModels({ dataDir, deployment, fetch: listing }).setSessionModel('s', 'local/big');
  await driver.stop('s');
  settingsDefault = undefined;
  await driver.ensure('s');
  assert.equal(await digest(snapshot), pinned, 'DEP-3: the pin is byte for byte what it was');
  assert.equal(JSON.parse(await readFile(join(snapshot, 'agent.json'), 'utf8')).model, undefined);
  assert.deepEqual(seen, [{ default: 'local/hand' }, { session: 'local/big' }], 'a cleared Settings default is removed before the next start');
  await assert.rejects(stat(join(dataDir, sessionDirName('s'), 'default-model')));
});
