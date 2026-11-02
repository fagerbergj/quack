import assert from 'node:assert/strict';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import { createDistributor, type HostDriver } from '../src/index.ts';

const TOKEN = 'admin';
const servers: Array<() => void> = [];
after(() => servers.forEach((close) => close()));

async function start(opts: { running?: string[]; sleeping?: string[]; waiting?: string[] }) {
  const existing = new Set(['idle', 'busy', 'asleep', 'asking']);
  const forgotten: string[] = [];
  const driver = {
    running: () => opts.running ?? [], wakes: async () => (opts.sleeping ?? []).map((key) => ({ key, at: 1 })),
    remove: async (key: string) => existing.delete(key), stop: async () => {}, setWake: async () => {},
  } as unknown as HostDriver;
  const server = createDistributor(driver, { retire: { token: TOKEN, waiting: async (k) => (opts.waiting ?? []).includes(k), forget: async (k) => void forgotten.push(k) } }).listen(0);
  await new Promise((r) => server.once('listening', r));
  servers.push(() => server.close());
  const del = (key: string, token = TOKEN) => fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/${encodeURIComponent(key)}`, { method: 'DELETE', headers: { authorization: `Bearer ${token}` } }).then((r) => r.status);
  return { del, forgotten, existing };
}

test('deleting an idle session removes its directory and index rows', async () => {
  const { del, forgotten, existing } = await start({});
  assert.equal(await del('idle'), 200);
  assert.deepEqual([forgotten, existing.has('idle')], [['idle'], false]);
  assert.equal(await del('idle'), 404);
});

test('a running, sleeping or person-waiting session is refused and kept', async () => {
  const { del, forgotten, existing } = await start({ running: ['busy'], sleeping: ['asleep'], waiting: ['asking'] });
  for (const key of ['busy', 'asleep', 'asking']) assert.equal(await del(key), 409, key);
  assert.deepEqual([forgotten, existing.size], [[], 4]);
});

test('deleting needs the admin token', async () => {
  const { del, existing } = await start({});
  assert.equal(await del('idle', 'wrong'), 401);
  assert.equal(existing.has('idle'), true);
});
