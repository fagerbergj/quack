import assert from 'node:assert/strict';
import type { AddressInfo } from 'node:net';
import { test } from 'node:test';
import { serveApp } from '@quack/sdk';
import { snapshotHistory } from '../src/trends/index.ts';
import { createSleeperClient, createSleeperService } from '../src/index.ts';

// The "Sleeper app routes" v5 contract (contracts:6fe21e34-ebc4-41a7-8de5-63a38c0d8749), asserted route by route.
const DAY = 86_400_000;
const START = Date.parse('2026-01-01T15:00:00Z');

async function setup(opts: { configured?: boolean; refuse?: number } = {}) {
  const submitted: Array<{ url: string; requestId: string }> = [];
  const doFetch = (async (u: string, init?: { body?: string }) => {
    if (u.startsWith('http://d/')) {
      submitted.push({ url: u, requestId: JSON.parse(init!.body!).requestId });
      return new Response('{}', { status: opts.refuse ?? 202 });
    }
    const path = new URL(u).pathname;
    return new Response(JSON.stringify(path === '/v1/state/nfl' ? { week: 9 } : path.endsWith('/rosters') || path.endsWith('/users') ? [] : path.includes('/matchups/') ? [] : { league_id: 'L1' }));
  }) as unknown as typeof fetch;
  const clock = { at: new Date(START) };
  const client = createSleeperClient({ fetch: doFetch, now: () => +clock.at });
  const configured = opts.configured ?? true;
  const service = serveApp(createSleeperService({ dbPath: ':memory:', client, fetch: doFetch, now: () => clock.at, ...(configured && { distributorUrl: 'http://d', owner: 'mallard', league: 'L1' }) }));
  await new Promise<void>((r) => service.listen(0, r));
  const base = `http://127.0.0.1:${(service.address() as AddressInfo).port}`;
  const call = (method: string, path: string) => fetch(base + path, { method });
  /** Takes one snapshot per day for n days from START. */
  const seed = async (n: number) => {
    for (let i = 0; i < n; i++) {
      clock.at = new Date(START + i * DAY);
      await call('POST', '/leagues/L1/snapshots');
    }
  };
  return { service, call, submitted, seed };
}

async function problem(res: Response, status: number, match?: RegExp) {
  assert.equal(res.status, status);
  assert.equal(res.headers.get('content-type'), 'application/problem+json');
  const body = (await res.json()) as { type: string; title: string; status: number; detail: string; error: string };
  assert.deepEqual([body.type, body.status, typeof body.title, body.error], ['about:blank', status, 'string', body.detail]);
  if (match) assert.match(body.detail, match);
}

test('a start answers 202 with only the session key, and the old flat path is gone', async () => {
  const { service, call, submitted } = await setup();
  try {
    const res = await call('POST', '/jobs/lineup/runs?week=8');
    assert.deepEqual([res.status, await res.json()], [202, { sessionKey: 'sleeper:L1:8:lineup' }]);
    assert.equal(submitted.length, 1);
    await problem(await call('POST', '/lineup'), 404);
    await problem(await call('POST', '/lineup?week=8'), 404);
  } finally {
    service.stop();
  }
});

test('a refusal from the distributor is a 502 problem naming it, not an accepted run', async () => {
  const { service, call } = await setup({ refuse: 409 });
  try {
    await problem(await call('POST', '/jobs/lineup/runs'), 502, /refused.*409/);
  } finally {
    service.stop();
  }
});

test('an unknown job is a 404, a bad week a 400, and an unconfigured deployment a 501, all problems', async () => {
  const { service, call, submitted } = await setup();
  const bare = await setup({ configured: false });
  try {
    await problem(await call('POST', '/jobs/scout/runs'), 404, /scout/);
    await problem(await call('POST', '/jobs/constructor/runs'), 404);
    for (const week of ['0', '-1', '1.5', 'x', '', '0x10', '1e1']) await problem(await call('POST', `/jobs/lineup/runs?week=${week}`), 400, /week/);
    assert.equal(submitted.length, 0);
    await problem(await bare.call('POST', '/jobs/lineup/runs'), 501);
    await problem(await bare.call('GET', '/'), 501, /no league/);
  } finally {
    service.stop();
    bare.service.stop();
  }
});

test('a requestId is the submission id so a retry joins the first run; a call without one is a new submission', async () => {
  const { service, call, submitted } = await setup();
  try {
    await call('POST', '/jobs/waivers/runs?requestId=abc');
    await call('POST', '/jobs/waivers/runs?requestId=abc');
    await call('POST', '/jobs/waivers/runs');
    await call('POST', '/jobs/waivers/runs');
    const ids = submitted.map((s) => s.requestId);
    assert.deepEqual(ids.slice(0, 2), ['abc', 'abc']);
    assert.match(ids[2]!, /^start:sleeper:L1:9:waivers:/);
    assert.deepEqual(new Set(submitted.map((s) => s.url)).size, 1, 'reruns share one session');
    await problem(await call('POST', '/jobs/waivers/runs?requestId='), 400, /requestId/);
  } finally {
    service.stop();
  }
});

test('a wrong method on a known path is a 405 problem with Allow, and an unknown path a 404', async () => {
  const { service, call } = await setup();
  try {
    const cases: Array<[string, string, string]> = [
      ['POST', '/', 'GET'], ['GET', '/jobs/lineup/runs', 'POST'], ['DELETE', '/leagues/L1/snapshots', 'GET, POST'], ['POST', '/leagues/L1/snapshots/2026-01-01', 'GET'],
    ];
    for (const [method, path, allow] of cases) {
      const res = await call(method, path);
      assert.equal(res.headers.get('allow'), allow, `${method} ${path}`);
      await problem(res, 405);
    }
    await problem(await call('GET', '/nope'), 404);
    await problem(await call('GET', '/jobs/lineup'), 404);
    await problem(await call('GET', '/leagues/L1'), 404);
  } finally {
    service.stop();
  }
});

test('the snapshot list is paged newest first, and the last page has no next token', async () => {
  const { service, call, seed } = await setup();
  try {
    await seed(5);
    const dates = async (q: string) => {
      const body = (await (await call('GET', `/leagues/L1/snapshots${q}`)).json()) as { snapshots: Array<{ date: string }>; nextPageToken?: string };
      return { dates: body.snapshots.map((s) => s.date), next: body.nextPageToken };
    };
    const first = await dates('?pageSize=2');
    assert.deepEqual(first.dates, ['2026-01-05', '2026-01-04']);
    const second = await dates(`?pageSize=2&pageToken=${first.next}`);
    assert.deepEqual(second.dates, ['2026-01-03', '2026-01-02']);
    const last = await dates(`?pageSize=2&pageToken=${second.next}`);
    assert.deepEqual([last.dates, last.next], [['2026-01-01'], undefined]);
    assert.deepEqual((await dates('?pageSize=5')).next, undefined, 'a page that ends exactly at the last row has no token');
    assert.equal((await dates('')).dates.length, 5, 'the default size is 100');
    const row = (await (await call('GET', '/leagues/L1/snapshots?pageSize=1')).json()) as { snapshots: Array<Record<string, unknown>> };
    assert.deepEqual(Object.keys(row.snapshots[0]!).sort(), ['date', 'takenAt']);
  } finally {
    service.stop();
  }
});

test('a bad pageSize or an unissued pageToken is a 400 problem', async () => {
  const { service, call, seed } = await setup();
  try {
    await seed(1);
    for (const size of ['0', '1001', '-1', 'x', '1.5', '']) await problem(await call('GET', `/leagues/L1/snapshots?pageSize=${size}`), 400, /pageSize/);
    assert.equal((await call('GET', '/leagues/L1/snapshots?pageSize=1000')).status, 200);
    for (const token of ['garbage', '!!', Buffer.from('2026-01-01x').toString('base64url'), Buffer.from('nope').toString('base64url'), `${Buffer.from('2026-01-01').toString('base64url')}=`]) {
      await problem(await call('GET', `/leagues/L1/snapshots?pageToken=${token}`), 400, /pageToken/);
    }
  } finally {
    service.stop();
  }
});

test('taking today\'s snapshot answers 200 with the stored row either way, and a missing day is a 404 problem', async () => {
  const { service, call } = await setup();
  try {
    await problem(await call('GET', '/leagues/L1/snapshots/2026-01-01'), 404);
    const taken = await call('POST', '/leagues/L1/snapshots');
    assert.equal(taken.status, 200);
    const row = await taken.json();
    assert.deepEqual(Object.keys(row).sort(), ['data', 'date', 'league', 'takenAt']);
    const again = await call('POST', '/leagues/L1/snapshots');
    assert.deepEqual([again.status, await again.json()], [200, row]);
    assert.deepEqual(await (await call('GET', '/leagues/L1/snapshots/2026-01-01')).json(), row);
    await problem(await call('GET', '/leagues/L1/snapshots/2026-01-02'), 404);
    await problem(await call('GET', '/leagues/L1/snapshots/yesterday'), 404);
    await problem(await call('GET', '/leagues/bad%zz/snapshots'), 400);
  } finally {
    service.stop();
  }
});

test('an upstream fault while taking a snapshot is a 502 problem', async () => {
  const client = createSleeperClient({ fetch: (async () => new Response('null')) as unknown as typeof fetch });
  const service = serveApp(createSleeperService({ dbPath: ':memory:', client }));
  await new Promise<void>((r) => service.listen(0, r));
  try {
    await problem(await fetch(`http://127.0.0.1:${(service.address() as AddressInfo).port}/leagues/L1/snapshots`, { method: 'POST' }), 502);
  } finally {
    service.stop();
  }
});

test('a pre-v24 bare-date pageToken is a 400, and the trends history walks every page of the new tokens', async () => {
  const { service, call, seed } = await setup();
  try {
    await seed(1001);
    await problem(await call('GET', `/leagues/L1/snapshots?pageToken=${Buffer.from('2026-01-01').toString('base64url')}`), 400, /pageToken/);
    const get = async (path: string) => (await call('GET', path)).json();
    const all = await snapshotHistory(get, '/leagues/L1/snapshots');
    assert.equal(all.length, 1001);
    assert.equal(new Set(all.map((s) => s.date)).size, 1001);
  } finally {
    service.stop();
  }
});
