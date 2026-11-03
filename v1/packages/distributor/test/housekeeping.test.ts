import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readdir, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import { createDistributor, processDriver, sessionDirName, type HostDriver } from '../src/index.ts';

const cleanups: Array<() => void> = [];
after(() => cleanups.forEach((fn) => fn()));

// A real driver over a temp data dir; `busy` names a session whose process answers /ping mid-run.
async function start(opts: { busy?: string; waiting?: string[] } = {}) {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-house-'));
  cleanups.push(() => void rm(dataDir, { recursive: true, force: true }));
  for (const key of ['idle', 'busy', 'asking']) {
    await mkdir(join(dataDir, sessionDirName(key)), { recursive: true });
    await writeFile(join(dataDir, sessionDirName(key), 'session-key'), key);
  }
  const upstream = createServer((_req, res) => res.end(JSON.stringify({ status: 'HealthyBusy' }))).listen(0);
  cleanups.push(() => upstream.close());
  const real = processDriver({ command: [], dataDir });
  const driver: HostDriver = { ...real, running: () => (opts.busy ? [opts.busy] : []), ensure: async () => `http://127.0.0.1:${(upstream.address() as AddressInfo).port}`, touch: async () => {}, setWake: async () => {} };
  const forgotten: string[] = [];
  const server = createDistributor(driver, { retire: { waiting: async (k) => (opts.waiting ?? []).includes(k), forget: async (k) => void forgotten.push(k) } }).listen(0);
  await new Promise((r) => server.once('listening', r));
  cleanups.push(() => server.close());
  const url = (path: string) => `http://127.0.0.1:${(server.address() as AddressInfo).port}${path}`;
  const call = async (method: string, path: string, body: unknown = {}, headers: Record<string, string> = { 'content-type': 'application/json' }) => {
    const res = await fetch(url(path), { method, headers, body: JSON.stringify(body) });
    return { status: res.status, body: (await res.json()) as { error?: string } };
  };
  const listed = async () => ((await (await fetch(url('/sessions'))).json()) as { sessions: Array<{ key: string; title?: string; archived?: boolean }> }).sessions;
  return { call, listed, forgotten, dataDir, sweep: () => server.sweep() };
}

test('renaming sets the title, and an empty title restores the default', async () => {
  const { call, listed } = await start();
  assert.equal((await call('PATCH', '/sessions/idle', { title: '  Nightly review  ' })).status, 200);
  assert.equal((await listed()).find((s) => s.key === 'idle')?.title, 'Nightly review');
  await call('PATCH', '/sessions/idle', { title: '' });
  assert.equal((await listed()).find((s) => s.key === 'idle')?.title, undefined);
});

test('archive flags the session in the listing and unarchive clears it', async () => {
  const { call, listed } = await start();
  assert.equal((await call('PATCH', '/sessions/idle', { archived: true })).status, 200);
  assert.equal((await listed()).find((s) => s.key === 'idle')?.archived, true);
  await call('PATCH', '/sessions/idle', { archived: false });
  assert.equal((await listed()).find((s) => s.key === 'idle')?.archived, undefined);
});

test('archive is refused for a session mid-run or waiting on a person, and rename still works', async () => {
  const { call, listed, sweep } = await start({ busy: 'busy', waiting: ['asking'] });
  await sweep();
  for (const key of ['busy', 'asking']) {
    const res = await call('PATCH', `/sessions/${key}`, { archived: true });
    assert.deepEqual([res.status, res.body.error], [409, "Can't archive a session that is running or waiting on you."]);
  }
  assert.equal((await call('PATCH', '/sessions/busy', { title: 'Still renamable' })).status, 200);
  assert.deepEqual((await listed()).filter((s) => s.archived), []);
});

test('delete removes the directory and index rows, and is refused while running', async () => {
  const { call, forgotten, dataDir } = await start({ busy: 'busy' });
  assert.equal((await call('POST', '/sessions/busy/delete')).status, 409);
  assert.equal((await call('POST', '/sessions/idle/delete')).status, 200);
  assert.deepEqual(forgotten, ['idle']);
  assert.equal((await readdir(dataDir)).includes(sessionDirName('idle')), false);
});

test('an unknown key is 404, a bad body 400, and a non-JSON caller 415', async () => {
  const { call } = await start();
  assert.equal((await call('PATCH', '/sessions/nope', { title: 'x' })).status, 404);
  assert.equal((await call('POST', '/sessions/nope/delete')).status, 404);
  assert.equal((await call('PATCH', '/sessions/idle', { title: 5 })).status, 400);
  assert.equal((await call('PATCH', '/sessions/idle', {})).status, 400);
  assert.equal((await call('POST', '/sessions/idle/delete', {}, { 'content-type': 'text/plain' })).status, 415);
});
