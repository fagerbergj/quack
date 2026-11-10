import assert from 'node:assert/strict';
import { openCatalog } from '@quack/catalog';
import { mkdir, mkdtemp, readdir, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import { createDistributor, processDriver, sessionDirName, type HostDriver } from '../src/index.ts';
import { PERSON, PROXY_SECRET } from './fixtures/proxy.ts';

const cleanups: Array<() => void> = [];
after(() => cleanups.forEach((fn) => fn()));

// A real driver over a temp data dir; `busy` names a session whose process answers /ping mid-run.
async function start(opts: { busy?: string; waiting?: string[] } = {}) {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-house-'));
  cleanups.push(() => void rm(dataDir, { recursive: true, force: true }));
  for (const key of ['idle', 'busy', 'asking']) {
    await mkdir(join(dataDir, sessionDirName(key)), { recursive: true });
    await writeFile(join(dataDir, sessionDirName(key), 'session-key'), key);
    await writeFile(join(dataDir, sessionDirName(key), 'agent'), '');
  }
  const upstream = createServer((req, res) => res.end(JSON.stringify(req.url === '/export' ? { transcript: [], artifacts: [] } : { status: 'HealthyBusy' }))).listen(0);
  cleanups.push(() => upstream.close());
  const real = processDriver({ command: [], dataDir });
  const driver: HostDriver = { ...real, running: () => (opts.busy ? [opts.busy] : []), ensure: async () => `http://127.0.0.1:${(upstream.address() as AddressInfo).port}`, touch: async () => {}, setWake: async () => {} };
  // The catalog the session channels would have filled: each key known, `busy` running, `waiting` with a pending ask.
  const catalog = openCatalog({ dataDir: await mkdtemp(join(tmpdir(), 'quack-house-catalog-')) });
  cleanups.push(() => void catalog.close());
  for (const key of ['idle', 'busy', 'asking']) {
    await catalog.publish(key, [
      { seq: 1, id: `session:${key}`, at: 0, type: 'session.created', data: { agent: 'quack', createdAt: 0 } },
      ...(key === opts.busy ? [{ seq: 2, id: 'run:1', at: 1, type: 'run.started' as const, data: { run: 1 } }] : []),
      ...((opts.waiting ?? []).includes(key) ? [{ seq: 2, id: 'ask', at: 1, type: 'ask.opened' as const, data: { id: 'a1', type: 'approval' as const, createdAt: 1, toolCall: { tool: 't', args: {} } } }] : []),
    ]);
  }
  const server = createDistributor(driver, { proxySecret: PROXY_SECRET, catalog }).listen(0);
  await new Promise((r) => server.once('listening', r));
  cleanups.push(() => server.close());
  const url = (path: string) => `http://127.0.0.1:${(server.address() as AddressInfo).port}${path}`;
  const call = async (method: string, path: string, body: unknown = {}, headers: Record<string, string> = { 'content-type': 'application/json' }) => {
    // The page reaches these routes through the proxy, which sets the caller's identity.
    const res = await fetch(url(path), { method, headers: { ...PERSON, ...headers }, ...(method !== 'GET' && { body: JSON.stringify(body) }) });
    return { status: res.status, type: res.headers.get('content-type'), body: (await res.json()) as { error?: string; detail?: string; title?: string; status?: number; state?: string } };
  };
  const listed = async () => ((await (await fetch(url('/sessions'))).json()) as { sessions: Array<{ key: string; title?: string; state: string }> }).sessions;
  return { url, call, listed, catalog, dataDir, sweep: () => server.sweep() };
}

test('the public listener serves the Sessions API from the catalog: rename, archive and unarchive', async () => {
  const { call, listed } = await start();
  assert.equal((await call('PATCH', '/sessions/idle', { title: '  Nightly review  ' })).status, 200);
  assert.equal((await listed()).find((s) => s.key === 'idle')?.title, 'Nightly review');
  assert.equal((await call('PATCH', '/sessions/idle', { state: 'archived' })).body.state, 'archived');
  assert.equal((await call('PATCH', '/sessions/idle', { state: 'stopped' })).body.state, 'stopped');
  assert.equal((await call('PATCH', '/sessions/idle', { archived: true })).status, 200, 'v1 archived is no longer a field; the write changes nothing');
});

test('archive is refused for a running session, and the title stays as it was', async () => {
  const { call, listed } = await start({ busy: 'busy' });
  assert.equal((await call('PATCH', '/sessions/busy', { title: 'Never', state: 'archived' })).status, 409);
  assert.equal((await listed()).find((s) => s.key === 'busy')?.title, undefined);
});

test('delete removes the directory and the catalog rows, and is refused while running or waiting on a person', async () => {
  const { call, catalog, dataDir } = await start({ busy: 'busy', waiting: ['asking'] });
  assert.equal((await call('DELETE', '/sessions/busy')).status, 409);
  assert.match(String((await call('DELETE', '/sessions/asking')).body.detail), /waiting on a person/);
  assert.equal((await call('DELETE', '/sessions/idle')).status, 200);
  assert.deepEqual([await catalog.getSession('idle'), await readdir(dataDir).then((d) => d.includes(sessionDirName('idle')))], [undefined, false]);
  assert.equal((await call('GET', '/sessions/idle')).status, 404);
});

test('an unknown key is 404, a bad state 422, and a non-JSON body 415', async () => {
  const { call } = await start();
  assert.equal((await call('PATCH', '/sessions/nope', { title: 'x' })).status, 404);
  assert.equal((await call('PATCH', '/sessions/idle', { state: 'running' })).status, 422);
  assert.equal((await call('PATCH', '/sessions/idle', { title: 'x' }, { 'content-type': 'text/plain' })).status, 415);
});

test('export downloads one file with the session metadata and what the process returns, and 404s an unknown key', async () => {
  const { url, call } = await start();
  await call('PATCH', '/sessions/idle', { title: 'Nightly review', state: 'archived' });
  const res = await fetch(url('/sessions/idle/export'));
  assert.equal(res.status, 200);
  assert.equal(res.headers.get('content-disposition'), 'attachment; filename="quack-idle.json"');
  const body = (await res.json()) as { session: object };
  // The metadata is the catalog's, so a rename or archive shows at once.
  assert.deepEqual(body.session, { key: 'idle', agent: '', title: 'Nightly review', archived: true, createdAt: new Date(0).toISOString(), lastActivity: new Date(0).toISOString() });
  assert.equal((await fetch(url('/sessions/nope/export'))).status, 404);
});
