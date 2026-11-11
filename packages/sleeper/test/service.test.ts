import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import type { AddressInfo } from 'node:net';
import { test } from 'node:test';
import { createSchedulesService } from '@quack/schedules';
import { primeSessionEndpoint, serveApp } from '@quack/sdk';
import { NotFound, createSleeperClient, createSleeperService, leagueDate, sessionClient } from '../src/index.ts';
import { ATTEMPTS, CONCURRENCY, TIMEOUT_MS } from '../src/client.ts';

const fixtures = JSON.parse(readFileSync(new URL('./fixtures/league.json', import.meta.url), 'utf8')) as Record<string, unknown>;

/** Recorded-fixture double of api.sleeper.app: logs every request, answers from the fixture file. */
function double() {
  const calls: Array<{ method: string; path: string }> = [];
  const doFetch = (async (input: string, init?: RequestInit) => {
    const path = new URL(input).pathname;
    calls.push({ method: init?.method ?? 'GET', path });
    return path in fixtures ? new Response(JSON.stringify(fixtures[path])) : new Response('null');
  }) as unknown as typeof fetch;
  return { calls, doFetch };
}

async function setup(clock: { at: Date }) {
  const api = double();
  const client = createSleeperClient({ fetch: api.doFetch, now: () => +clock.at });
  const service = serveApp(createSleeperService({ dbPath: ':memory:', client, now: () => clock.at }));
  await new Promise<void>((r) => service.listen(0, r));
  const base = `http://127.0.0.1:${(service.address() as AddressInfo).port}`;
  const call = (method: string, path: string) => fetch(base + path, { method });
  return { api, service, call };
}

test('a snapshot is taken and stored', async () => {
  const clock = { at: new Date('2026-11-01T15:00:00Z') };
  const { service, call } = await setup(clock);
  try {
    assert.deepEqual(await (await call('GET', '/leagues/L1/snapshots')).json(), { snapshots: [] });
    const taken = await (await call('POST', '/leagues/L1/snapshots')).json();
    assert.equal(taken.date, '2026-11-01');
    assert.equal(taken.data.league.name, 'Pond League');
    assert.equal(taken.data.matchups.length, 2);
    // Only rostered players are kept, as slivers.
    assert.deepEqual(Object.keys(taken.data.players).sort(), ['4034', '6794']);
    assert.equal(taken.data.players['4034'].injury, 'Questionable');
    assert.deepEqual(await (await call('GET', '/leagues/L1/snapshots/2026-11-01')).json(), taken);
  } finally {
    service.stop();
  }
});

test('a second snapshot on the same day is idempotent, and history lists newest first', async () => {
  const clock = { at: new Date('2026-11-01T15:00:00Z') };
  const { api, service, call } = await setup(clock);
  try {
    const first = await (await call('POST', '/leagues/L1/snapshots')).json();
    const seen = api.calls.length;
    clock.at = new Date('2026-11-01T22:00:00Z');
    const again = await (await call('POST', '/leagues/L1/snapshots')).json();
    assert.deepEqual(again, first);
    assert.equal(api.calls.length, seen, 'a stored day does not touch Sleeper');
    clock.at = new Date('2026-11-02T15:00:00Z');
    await call('POST', '/leagues/L1/snapshots');
    const { snapshots } = await (await call('GET', '/leagues/L1/snapshots')).json();
    assert.deepEqual(snapshots.map((s: { date: string }) => s.date), ['2026-11-02', '2026-11-01']);
  } finally {
    service.stop();
  }
});

test('the snapshot day follows America/New_York, not UTC', () => {
  assert.equal(leagueDate(new Date('2026-11-02T03:30:00Z')), '2026-11-01');
});

test('a malformed league id escape is a 400', async () => {
  const { service, call } = await setup({ at: new Date('2026-11-01T15:00:00Z') });
  try {
    assert.equal((await call('GET', '/leagues/bad%zz/snapshots')).status, 400);
  } finally {
    service.stop();
  }
});

test('the page routes are relative to the mount: the league id, and an unknown path is a 404', async () => {
  const { service, call } = await setup({ at: new Date('2026-11-01T15:00:00Z') });
  try {
    assert.equal((await call('GET', '/')).status, 501, 'no league configured');
    assert.equal((await call('GET', '/nope')).status, 404);
  } finally {
    service.stop();
  }
});

test('the client issues only GETs and honours the players cache', async () => {
  const clock = { at: new Date('2026-11-01T15:00:00Z') };
  const api = double();
  const client = createSleeperClient({ fetch: api.doFetch, now: () => +clock.at });
  await client.state();
  await client.league('L1');
  await client.users('L1');
  await client.rosters('L1');
  await client.matchups('L1', 9);
  await client.draftPicks('D1');
  await client.players();
  await client.players();
  clock.at = new Date(+clock.at + 23 * 3_600_000);
  await client.players();
  assert.ok(api.calls.every((c) => c.method === 'GET'));
  assert.equal(api.calls.filter((c) => c.path === '/v1/players/nfl').length, 1);
  clock.at = new Date(+clock.at + 2 * 3_600_000);
  await client.players();
  assert.equal(api.calls.filter((c) => c.path === '/v1/players/nfl').length, 2, 'refetched after the day-long TTL');
});

test('a null body is not found and is never cached', async () => {
  const api = double();
  const client = createSleeperClient({ fetch: api.doFetch });
  await assert.rejects(client.league('nope'), NotFound);
  await assert.rejects(client.league('nope'), NotFound);
  assert.equal(api.calls.length, 2);
});

const reply = (status: number, body: unknown = {}, headers: Record<string, string> = {}) => new Response(JSON.stringify(body), { status, headers });

test('a 429 waits its Retry-After, a 5xx backs off doubling, and the call gives up after ATTEMPTS', async () => {
  const waits: number[] = [];
  const queue = [reply(429, {}, { 'retry-after': '7' }), reply(503), reply(200, { week: 9 })];
  const client = createSleeperClient({ fetch: (async () => queue.shift()!) as unknown as typeof fetch, sleep: async (ms) => void waits.push(ms) });
  assert.deepEqual(await client.state(), { week: 9 });
  assert.deepEqual(waits, [7000, 2000]);
  waits.length = 0;
  let calls = 0;
  const busy = createSleeperClient({ fetch: (async () => (calls++, reply(429))) as unknown as typeof fetch, sleep: async (ms) => void waits.push(ms) });
  await assert.rejects(busy.state(), new RegExp(`returned 429 after ${ATTEMPTS} attempts`));
  assert.deepEqual([calls, waits], [ATTEMPTS, [1000, 2000, 4000]]);
  calls = 0;
  const missing = createSleeperClient({ fetch: (async () => (calls++, reply(404))) as unknown as typeof fetch, sleep: async () => {} });
  await assert.rejects(missing.league('x'), NotFound);
  assert.equal(calls, 1, 'a 404 is an answer, not retried');
});

test('a request that hangs past the timeout is aborted and retried', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let calls = 0;
  const doFetch = ((_u: string, init: RequestInit) => {
    if (++calls > 1) return Promise.resolve(reply(200, { week: 9 }));
    return new Promise((_, reject) => init.signal!.addEventListener('abort', () => reject(init.signal!.reason)));
  }) as unknown as typeof fetch;
  const client = createSleeperClient({ fetch: doFetch, sleep: async () => {} });
  const state = client.state();
  await new Promise(setImmediate);
  t.mock.timers.tick(TIMEOUT_MS);
  assert.deepEqual(await state, { week: 9 });
  assert.equal(calls, 2);
});

test('at most CONCURRENCY requests are in flight, and concurrent callers of one path share one request', async () => {
  let inflight = 0;
  let peak = 0;
  const paths: string[] = [];
  const doFetch = (async (u: string) => {
    paths.push(new URL(u).pathname);
    peak = Math.max(peak, ++inflight);
    await new Promise(setImmediate);
    inflight--;
    return reply(200, []);
  }) as unknown as typeof fetch;
  const client = createSleeperClient({ fetch: doFetch });
  await Promise.all([...Array.from({ length: 30 }, (_, i) => client.matchups('L1', i + 1)), ...Array.from({ length: 5 }, () => client.rosters('L1'))]);
  assert.equal(peak, CONCURRENCY);
  assert.equal(paths.filter((p) => p === '/v1/league/L1/rosters').length, 1);
  assert.equal(paths.length, 31);
});

test('the app serves its players dump from its day-long copy, and a session client reads it there instead of from Sleeper', async (t) => {
  const { api, service, call } = await setup({ at: new Date('2026-11-01T15:00:00Z') });
  try {
    const first = await (await call('GET', '/players')).json();
    await call('GET', '/players');
    assert.equal(first['4034'].injury_status, 'Questionable');
    assert.equal(api.calls.filter((c) => c.path === '/v1/players/nfl').length, 1);
  } finally {
    service.stop();
  }
  const seen: string[] = [];
  t.mock.method(globalThis, 'fetch', async (u: string, init?: RequestInit) => (seen.push(`${u} ${new Headers(init?.headers).get('authorization')}`), reply(200, { '1': { first_name: 'A' } })));
  primeSessionEndpoint({ url: 'http://index', token: 'tok' });
  t.after(() => primeSessionEndpoint(undefined));
  const client = sessionClient();
  assert.deepEqual(await client.players(), { '1': { first_name: 'A' } });
  await client.players();
  assert.deepEqual(seen, ['http://index/apps/sleeper/players Bearer tok']);
  assert.equal(sessionClient(), client, 'one client, and one cache, per session process');
});
