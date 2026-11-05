import assert from 'node:assert/strict';
import { chmod, mkdtemp, mkdir, writeFile } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { DatabaseSync } from 'node:sqlite';
import { join } from 'node:path';
import { afterEach, test } from 'node:test';
import { serveApp } from '@quack/sdk';
import { createSchedulesService } from '../src/index.ts';

type Call = { url: string; body: { input: string; requestId: string } };

let clock = new Date('2026-01-01T00:00:00Z');
let calls: Call[] = [];
let ok = true;
let failClockOnce = false;
let onFetch: (() => void) | undefined;
const open: Array<{ stop(): void }> = [];
afterEach(() => {
  open.splice(0).forEach((s) => s.stop());
  calls = [];
  ok = true;
  onFetch = undefined;
  failClockOnce = false;
});

const fakeFetch = (async (url: string, init: { body: string }) => {
  calls.push({ url, body: JSON.parse(init.body) });
  onFetch?.();
  return { ok };
}) as unknown as typeof fetch;

async function start(dbPath: string, agentsDir?: string, tickMs?: number) {
  const svc = createSchedulesService({ distributorUrl: 'http://d', dbPath, agentsDir, now: () => {
      if (failClockOnce) {
        failClockOnce = false;
        throw new Error('database is busy');
      }
      return clock;
    }, fetch: fakeFetch, tickMs });
  const server = serveApp(svc).listen(0);
  open.push({ stop: () => (server.close(), svc.stop()) });
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const api = (method: string, path: string, body?: unknown) =>
    fetch(base + path, { method, body: body ? JSON.stringify(body) : undefined }).then(async (r) => ({ status: r.status, json: (await r.json()) as any }));
  return { svc, api, base };
}

const daily = { id: 'daily', agent: 'a', cron: '0 9 * * *', sessionKey: 'daily-{time}', input: 'go' };
const dbFile = async () => join(await mkdtemp(join(tmpdir(), 'sched-')), 's.db');

test('fires at the occurrence with a deterministic request id, once', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  assert.equal((await api('POST', '/', daily)).status, 201);
  await svc.tick();
  assert.equal(calls.length, 0);
  clock = new Date('2026-01-01T09:00:00Z');
  await svc.tick();
  await svc.tick();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, `http://d/sessions/${encodeURIComponent('daily-2026-01-01T09:00:00.000Z')}/invocations?agent=a`);
  assert.deepEqual(calls[0].body, { input: 'go', requestId: 'schedule:daily:2026-01-01T09:00:00.000Z' });
  const [listed] = (await api('GET', '/')).json.schedules;
  assert.equal(listed.lastRun, '2026-01-01T09:00:00.000Z');
  assert.equal(listed.nextRun, '2026-01-02T09:00:00.000Z');
});

test('a failed submission is retried, not recorded', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  await api('POST', '/', daily);
  clock = new Date('2026-01-01T09:00:00Z');
  ok = false;
  await svc.tick();
  ok = true;
  await svc.tick();
  assert.equal(calls.length, 2);
  assert.equal(calls[0].body.requestId, calls[1].body.requestId);
  assert.equal((await api('GET', '/')).json.schedules[0].lastRun, '2026-01-01T09:00:00.000Z');
});

test('after downtime only the newest useful miss fires and the rest are recorded missed', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  await api('POST', '/', { ...daily, staleMs: 6 * 3600_000 });
  clock = new Date('2026-01-04T10:00:00Z');
  await svc.tick();
  assert.deepEqual(calls.map((c) => c.body.requestId), ['schedule:daily:2026-01-04T09:00:00.000Z']);
  const [listed] = (await api('GET', '/')).json.schedules;
  assert.deepEqual(listed.missedRuns, ['2026-01-01T09:00:00.000Z', '2026-01-02T09:00:00.000Z', '2026-01-03T09:00:00.000Z']);
});

test('a newest miss past its window is recorded missed, not fired', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  await api('POST', '/', { ...daily, staleMs: 3600_000 });
  clock = new Date('2026-01-02T12:00:00Z');
  await svc.tick();
  assert.equal(calls.length, 0);
  assert.deepEqual((await api('GET', '/')).json.schedules[0].missedRuns, ['2026-01-01T09:00:00.000Z', '2026-01-02T09:00:00.000Z']);
});

test('the default window is the next occurrence, and explicit times honor a deadline', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T00:00:00Z');
  await api('POST', '/', { id: 'once', agent: 'a', times: ['2026-01-01T01:00:00Z'], sessionKey: 'once', input: 'x', deadline: '2026-01-01T01:30:00Z' });
  await api('POST', '/', { id: 'two', agent: 'a', times: ['2026-01-01T01:00:00Z', '2026-01-01T02:00:00Z'], sessionKey: 'two', input: 'x' });
  clock = new Date('2026-01-01T02:00:00Z');
  await svc.tick();
  assert.deepEqual(calls.map((c) => c.body.requestId), ['schedule:two:2026-01-01T02:00:00.000Z']);
  const byId = Object.fromEntries((await api('GET', '/')).json.schedules.map((s: any) => [s.id, s]));
  assert.deepEqual(byId.once.missedRuns, ['2026-01-01T01:00:00.000Z']);
  assert.deepEqual(byId.two.missedRuns, ['2026-01-01T01:00:00.000Z']);
});

test('schedules and their history survive a restart', async () => {
  const path = await dbFile();
  const first = await start(path);
  clock = new Date('2026-01-01T08:00:00Z');
  await first.api('POST', '/', daily);
  clock = new Date('2026-01-01T09:00:00Z');
  await first.svc.tick();
  first.svc.stop();
  open.length = 0;

  clock = new Date('2026-01-01T09:05:00Z');
  const second = await start(path);
  await second.svc.tick();
  assert.equal(calls.length, 1, 'the recorded occurrence is not fired again');
  assert.equal((await second.api('GET', '/')).json.schedules[0].lastRun, '2026-01-01T09:00:00.000Z');
});

test('agent definitions register schedules bound to their agent, and removal unregisters them', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'agents-'));
  await mkdir(join(dir, 'sleeper'));
  const def = (schedules: unknown[]) => writeFile(join(dir, 'sleeper', 'agent.json'), JSON.stringify({ name: 'sleeper', version: 1, schedules }));
  await def([{ id: 'weekly', cron: '0 9 * * 1', sessionKey: 'weekly', input: 'run' }, { id: 'bad', sessionKey: 'k', input: 'x' }]);
  const { svc, api } = await start(':memory:', dir);
  clock = new Date('2026-01-05T08:00:00Z');
  await svc.tick();
  clock = new Date('2026-01-05T09:00:00Z');
  await svc.tick();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, 'http://d/sessions/weekly/invocations?agent=sleeper');
  assert.equal((await api('DELETE', '/sleeper%2Fweekly')).status, 404);
  await def([]);
  await svc.tick();
  assert.deepEqual((await api('GET', '/')).json.schedules, []);
});

test('a cron in America/New_York keeps its wall-clock time across the DST change', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-03-07T12:00:00Z');
  await api('POST', '/', { ...daily, tz: 'America/New_York' });
  clock = new Date('2026-03-08T14:00:00Z');
  await svc.tick();
  clock = new Date('2026-03-09T13:00:00Z');
  await svc.tick();
  // Clocks spring forward on Mar 8: 09:00 EST was 14:00Z, 09:00 EDT is 13:00Z (UTC would drift an hour).
  assert.deepEqual(calls.map((c) => c.body.requestId), ['schedule:daily:2026-03-08T13:00:00.000Z', 'schedule:daily:2026-03-09T13:00:00.000Z']);
  assert.equal((await api('POST', '/', { ...daily, id: 'z', tz: 'Mars/Base' })).status, 400);
});

test('an exhausted one-shot whose moment passed long ago is recorded missed, not fired', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T00:00:00Z');
  await api('POST', '/', { id: 'once', agent: 'a', times: ['2026-01-01T01:00:00Z'], sessionKey: 'once', input: 'x' });
  clock = new Date('2026-01-02T01:00:00Z');
  await svc.tick();
  assert.equal(calls.length, 0);
  assert.deepEqual((await api('GET', '/')).json.schedules[0].missedRuns, ['2026-01-01T01:00:00.000Z']);
});

test('a one-shot registered for the moment it is written fires once, and re-registering it does not fire it again', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T10:00:00Z');
  const once = { id: 'now', agent: 'a', times: ['2026-01-01T10:00:00Z'], sessionKey: 'now', input: 'x' };
  await api('POST', '/', once);
  await svc.tick();
  assert.equal(calls.length, 1);
  await api('POST', '/', { ...once, input: 'y' });
  await svc.tick();
  assert.equal(calls.length, 1);
});

test('a one-shot registered after its time but inside its window fires; a recorded missed one is not retried', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T10:30:00Z');
  const late = { id: 'late', agent: 'a', times: ['2026-01-01T10:00:00Z'], deadline: '2026-01-01T12:00:00Z', sessionKey: 'late', input: 'x' };
  await api('POST', '/', late);
  await svc.tick();
  assert.equal(calls.length, 1);
  await api('POST', '/', { id: 'gone', agent: 'a', times: ['2026-01-01T09:00:00Z'], deadline: '2026-01-01T09:30:00Z', sessionKey: 'gone', input: 'x' });
  await svc.tick();
  await api('POST', '/', { id: 'gone', agent: 'a', times: ['2026-01-01T09:00:00Z'], deadline: '2026-01-01T11:00:00Z', sessionKey: 'gone', input: 'x' });
  await svc.tick();
  assert.equal(calls.length, 1, 'past its window it was recorded missed, and a wider window later does not revive it');
});

test('a tick that throws is logged and the loop keeps running', async () => {
  const { svc, api } = await start(':memory:', undefined, 1);
  clock = new Date('2026-01-01T08:00:00Z');
  await api('POST', '/', daily);
  clock = new Date('2026-01-01T09:00:00Z');
  const fired = new Promise<void>((r) => (onFetch = r));
  failClockOnce = true;
  await svc.start?.();
  await fired;
  assert.equal(calls.length, 1);
});

test('an unreadable agents dir or agent.json keeps stored schedules and their history', { skip: process.getuid?.() === 0 }, async () => {
  const dir = await mkdtemp(join(tmpdir(), 'agents-'));
  await mkdir(join(dir, 'sleeper'));
  const file = join(dir, 'sleeper', 'agent.json');
  await writeFile(file, JSON.stringify({ name: 'sleeper', version: 1, schedules: [{ id: 'weekly', cron: '0 9 * * 1', sessionKey: 'weekly', input: 'run' }] }));
  const { svc, api } = await start(':memory:', dir);
  clock = new Date('2026-01-05T08:00:00Z');
  await svc.tick();
  for (const [target, mode] of [[dir, 0o755], [file, 0o644]] as const) {
    await chmod(target, 0o000);
    clock = new Date(+clock + 7 * 86400_000 + 3600_000);
    await svc.tick();
    await chmod(target, mode);
    assert.equal((await api('GET', '/')).json.schedules.length, 1);
    await svc.tick();
  }
  assert.deepEqual(calls.map((c) => c.body.requestId), ['schedule:sleeper/weekly:2026-01-12T09:00:00.000Z', 'schedule:sleeper/weekly:2026-01-19T09:00:00.000Z']);
});

test('an api schedule can be deleted, and an agent-declared id cannot be overwritten', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'agents-'));
  await mkdir(join(dir, 'sleeper'));
  await writeFile(join(dir, 'sleeper', 'agent.json'), JSON.stringify({ name: 'sleeper', version: 1, schedules: [{ id: 'weekly', cron: '0 9 * * 1', sessionKey: 'weekly', input: 'run' }] }));
  const { svc, api } = await start(':memory:', dir);
  await svc.tick();
  assert.equal((await api('POST', '/', { ...daily, id: 'sleeper/weekly' })).status, 409);
  await api('POST', '/', daily);
  assert.equal((await api('DELETE', '/daily')).status, 200);
  assert.deepEqual((await api('GET', '/')).json.schedules.map((s: any) => s.id), ['sleeper/weekly']);
});

test('the API rejects bad definitions and an oversized body', async () => {
  const { api, base } = await start(':memory:');
  assert.equal((await api('POST', '/', { ...daily, cron: 'nope' })).status, 400);
  assert.equal((await api('POST', '/', { ...daily, times: ['2026-01-01T00:00:00Z'] })).status, 400);
  assert.equal((await api('DELETE', '/daily')).status, 404);
  const big = await fetch(base + '/', { method: 'POST', body: 'x'.repeat(2 << 20) });
  assert.deepEqual([big.status, big.headers.get('content-type')], [413, 'application/problem+json']);
});

test('the retention sweep deletes only sessions idle past their agent retention', async () => {
  const sessions = [
    { key: 'old', running: false, lastActivity: '2026-01-01T00:00:00Z', retention: { idleDays: 14 } },
    { key: 'recent', running: false, lastActivity: '2026-01-20T00:00:00Z', retention: { idleDays: 14 } },
    { key: 'keep-forever', running: false, lastActivity: '2025-01-01T00:00:00Z' },
    { key: 'home:main', running: false, lastActivity: '2025-01-01T00:00:00Z', retention: { idleDays: 14 } },
    { key: 'running', running: true, lastActivity: '2026-01-01T00:00:00Z', retention: { idleDays: 14 } },
  ];
  const deletes: string[] = [];
  const asked: string[] = [];
  const fetchStub = (async (url: string, init?: { method?: string; headers?: Record<string, string> }) => {
    if (init?.method !== 'DELETE') {
      // Two rows to a page, running sessions filtered out as the request asks.
      const q = new URL(url).searchParams;
      asked.push(q.get('filter') ?? '');
      const rows = sessions.filter((s) => !s.running);
      const from = Number(q.get('pageToken') ?? 0);
      return { ok: true, json: async () => ({ sessions: rows.slice(from, from + 2), ...(from + 2 < rows.length && { nextPageToken: String(from + 2) }) }) };
    }
    assert.equal(init.headers?.authorization, 'Bearer admin');
    deletes.push(decodeURIComponent(url.split('/sessions/')[1]));
    return { ok: true, status: 200 };
  }) as unknown as typeof fetch;
  clock = new Date('2026-01-25T00:00:00Z');
  const svc = createSchedulesService({ distributorUrl: 'http://d', dbPath: ':memory:', distributorToken: 'admin', retentionSweep: true, now: () => clock, fetch: fetchStub });
  open.push(svc);
  await svc.tick();
  assert.deepEqual(deletes, ['old']);
  assert.deepEqual(asked, ['running:false', 'running:false'], 'the four stopped sessions took two pages');
  await svc.tick();
  assert.deepEqual(deletes, ['old'], 'the automatic sweep runs once a day');
});

test('a retention sweep whose distributor fetch fails is retried next tick, then not again for a day', async (t) => {
  t.mock.method(console, 'error', () => {});
  let up = false;
  let listings = 0;
  const fetchStub = (async () => {
    listings++;
    if (!up) throw new TypeError('fetch failed');
    return { ok: true, json: async () => ({ sessions: [] }) };
  }) as unknown as typeof fetch;
  clock = new Date('2026-01-25T00:00:00Z');
  const svc = createSchedulesService({ distributorUrl: 'http://d', dbPath: ':memory:', distributorToken: 'admin', retentionSweep: true, now: () => clock, fetch: fetchStub });
  open.push(svc);
  await svc.tick();
  assert.equal(listings, 1);
  up = true;
  await svc.tick();
  assert.equal(listings, 2, 'the failed sweep is retried on the next tick');
  await svc.tick();
  assert.equal(listings, 2, 'a successful sweep is not repeated within a day');
  clock = new Date('2026-01-26T00:00:00Z');
  await svc.tick();
  assert.equal(listings, 3);
});

test('a registration without an agent is a 400 naming the field', async () => {
  const { api } = await start(':memory:');
  const { agent, ...noAgent } = daily;
  const res = await api('POST', '/', noAgent);
  assert.equal(res.status, 400);
  assert.match(res.json.error, /agent/);
  assert.deepEqual((await api('GET', '/')).json.schedules, []);
});

test('a stored schedule without an agent loads, never fires, and can still be deleted', async (t) => {
  t.mock.method(console, 'error', () => {});
  const db = await dbFile();
  const old = new DatabaseSync(db);
  old.exec(`CREATE TABLE schedules (id TEXT PRIMARY KEY, source TEXT NOT NULL, def TEXT NOT NULL, created TEXT NOT NULL)`);
  const { agent, ...legacy } = { ...daily, id: 'sleeper/snapshot/L1' };
  old.prepare(`INSERT INTO schedules VALUES (?, 'api', ?, ?)`).run(legacy.id, JSON.stringify(legacy), '2026-01-01T00:00:00.000Z');
  old.close();
  clock = new Date('2026-01-01T09:00:00Z');
  const { svc, api } = await start(db);
  await svc.tick();
  await svc.tick();
  assert.equal(calls.length, 0);
  assert.equal((await api('GET', '/')).json.schedules.length, 1);
  assert.equal((await api('DELETE', `/${encodeURIComponent(legacy.id)}`)).status, 200);
  assert.deepEqual((await api('GET', '/')).json.schedules, []);
});
