import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import type { AddressInfo } from 'node:net';
import { test } from 'node:test';
import { createSchedulesService } from '@quack/schedules';
import { NotFound, createSleeperClient, createSleeperService, leagueDate, registerSnapshotSchedule, registerUntilDone } from '../src/index.ts';

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

const TOKEN = 't';

async function setup(clock: { at: Date }) {
  const api = double();
  const client = createSleeperClient({ fetch: api.doFetch, now: () => +clock.at });
  const service = createSleeperService({ serviceToken: TOKEN, dbPath: ':memory:', client, now: () => clock.at });
  await new Promise<void>((r) => service.listen(0, r));
  const base = `http://127.0.0.1:${(service.address() as AddressInfo).port}`;
  const call = (method: string, path: string) => fetch(base + path, { method, headers: { authorization: `Bearer ${TOKEN}` } });
  return { api, service, call };
}

test('a snapshot is taken and stored', async () => {
  const clock = { at: new Date('2026-11-01T15:00:00Z') };
  const { service, call } = await setup(clock);
  try {
    assert.equal((await call('GET', '/leagues/L1/snapshots/latest')).status, 404);
    const taken = await (await call('POST', '/leagues/L1/snapshots')).json();
    assert.equal(taken.date, '2026-11-01');
    assert.equal(taken.data.league.name, 'Pond League');
    assert.equal(taken.data.matchups.length, 2);
    // Only rostered players are kept, as slivers.
    assert.deepEqual(Object.keys(taken.data.players).sort(), ['4034', '6794']);
    assert.equal(taken.data.players['4034'].injury, 'Questionable');
    const latest = await (await call('GET', '/leagues/L1/snapshots/latest')).json();
    assert.deepEqual(latest, taken);
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
  const { service, call } = await setup({ at: new Date() });
  try {
    assert.equal((await call('GET', '/leagues/bad%zz/snapshots')).status, 400);
  } finally {
    service.stop();
  }
});

test('the real schedules service lists the registered snapshot schedule', async () => {
  const schedules = createSchedulesService({ serviceToken: 's', distributorUrl: 'http://unused', dbPath: ':memory:' });
  await new Promise<void>((r) => schedules.listen(0, r));
  const url = `http://127.0.0.1:${(schedules.address() as AddressInfo).port}`;
  try {
    // Twice: a restart re-registers the same id.
    await registerSnapshotSchedule({ url, token: 's' }, 'L1');
    await registerSnapshotSchedule({ url, token: 's' }, 'L1');
    const { schedules: listed } = await (await fetch(`${url}/schedules`, { headers: { authorization: 'Bearer s' } })).json();
    assert.equal(listed.length, 1);
    assert.partialDeepStrictEqual(listed[0], { id: 'sleeper/snapshot/L1', cron: '0 4 * * *', tz: 'America/New_York', sessionKey: 'sleeper:snapshot:{time}', input: 'snapshot L1', source: 'api' });
    assert.ok(listed[0].nextRun);
  } finally {
    schedules.stop();
  }
});

test('the service rejects callers without the token', async () => {
  const { service } = await setup({ at: new Date() });
  try {
    const base = `http://127.0.0.1:${(service.address() as AddressInfo).port}`;
    assert.equal((await fetch(`${base}/leagues/L1/snapshots`)).status, 401);
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
  await client.transactions('L1', 9);
  await client.drafts('L1');
  await client.draft('D1');
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

test('registration retries with backoff until the schedules service is up, then registers once', async (t) => {
  t.mock.method(console, 'error', () => {});
  let attempts = 0;
  const posted: string[] = [];
  const doFetch = (async (_url: string, init: RequestInit) => {
    if (++attempts < 3) throw new Error('connection refused');
    posted.push(JSON.parse(init.body as string).id);
    return new Response('{}', { status: 201 });
  }) as unknown as typeof fetch;
  const delays: number[] = [];
  await registerUntilDone({ url: 'http://schedules', token: 's', fetch: doFetch }, 'L1', async (ms) => void delays.push(ms));
  assert.deepEqual(delays, [1000, 2000]);
  assert.deepEqual(posted, ['sleeper/snapshot/L1']);
});

test('the snapshot job registers in New York time', async () => {
  let sent: any;
  const doFetch = (async (url: string, init: RequestInit) => ((sent = { url, body: JSON.parse(init.body as string), auth: (init.headers as any).authorization }), new Response('{}', { status: 201 }))) as unknown as typeof fetch;
  await registerSnapshotSchedule({ url: 'http://schedules', token: 's', fetch: doFetch }, 'L1');
  assert.equal(sent.url, 'http://schedules/schedules');
  assert.equal(sent.auth, 'Bearer s');
  assert.equal(sent.body.tz, 'America/New_York');
  assert.equal(sent.body.id, 'sleeper/snapshot/L1');
  await assert.rejects(registerSnapshotSchedule({ url: 'http://schedules', token: 's', fetch: (async () => new Response('', { status: 500 })) as unknown as typeof fetch }, 'L1'));
});
