import assert from 'node:assert/strict';
import { mkdtemp } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
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

async function start(dbPath: string, tickMs?: number) {
  const svc = createSchedulesService({ distributorUrl: 'http://d', dbPath, now: () => {
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
    fetch(base + path, { method, body: body ? JSON.stringify(body) : undefined }).then(async (r) => {
      const text = await r.text();
      return { status: r.status, type: r.headers.get('content-type'), allow: r.headers.get('allow'), text, json: (text ? JSON.parse(text) : undefined) as any };
    });
  return { svc, api, base };
}

// The path carries the id and the body does not.
const put = (api: Awaited<ReturnType<typeof start>>['api'], { id, ...def }: { id: string } & Record<string, unknown>) => api('PUT', `/${encodeURIComponent(id)}`, def);
const daily = { id: 'daily', agent: 'a', cron: '0 9 * * *', sessionKey: 'daily', input: 'go' };
const dbFile = async () => join(await mkdtemp(join(tmpdir(), 'sched-')), 's.db');

test('fires at the occurrence with a deterministic request id, once', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  assert.equal((await put(api, daily)).status, 201);
  await svc.tick();
  assert.equal(calls.length, 0);
  clock = new Date('2026-01-01T09:00:00Z');
  await svc.tick();
  await svc.tick();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, 'http://d/sessions/daily/invocations?agent=a');
  assert.deepEqual(calls[0].body, { input: 'go', requestId: 'schedule:daily:2026-01-01T09:00:00.000Z' });
  const [listed] = (await api('GET', '/')).json.schedules;
  assert.equal(listed.lastRun, '2026-01-01T09:00:00.000Z');
  assert.equal(listed.nextRun, '2026-01-02T09:00:00.000Z');
});

test('a failed submission is retried, not recorded', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  await put(api, daily);
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
  await put(api, daily);
  clock = new Date('2026-01-04T10:00:00Z');
  await svc.tick();
  assert.deepEqual(calls.map((c) => c.body.requestId), ['schedule:daily:2026-01-04T09:00:00.000Z']);
  const [listed] = (await api('GET', '/')).json.schedules;
  assert.deepEqual(listed.missedRuns, ['2026-01-01T09:00:00.000Z', '2026-01-02T09:00:00.000Z', '2026-01-03T09:00:00.000Z']);
});

test('a newest miss past its window is recorded missed, not fired', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  await put(api, { ...daily, deadline: '2026-01-02T10:00:00Z' });
  clock = new Date('2026-01-02T12:00:00Z');
  await svc.tick();
  assert.equal(calls.length, 0);
  assert.deepEqual((await api('GET', '/')).json.schedules[0].missedRuns, ['2026-01-01T09:00:00.000Z', '2026-01-02T09:00:00.000Z']);
});

test('the default window is the next occurrence, and explicit times honor a deadline', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T00:00:00Z');
  await put(api, { id: 'once', agent: 'a', times: ['2026-01-01T01:00:00Z'], sessionKey: 'once', input: 'x', deadline: '2026-01-01T01:30:00Z' });
  await put(api, { id: 'two', agent: 'a', times: ['2026-01-01T01:00:00Z', '2026-01-01T02:00:00Z'], sessionKey: 'two', input: 'x' });
  clock = new Date('2026-01-01T02:00:00Z');
  await svc.tick();
  assert.deepEqual(calls.map((c) => c.body.requestId), ['schedule:two:2026-01-01T02:00:00.000Z']);
  const listed = (await api('GET', '/')).json.schedules;
  assert.deepEqual(listed.map((s: any) => s.id), [], 'both one-shots are spent: once missed, two fired its last time');
  assert.deepEqual(calls.length, 1);
});

test('a spent one-shot is deleted while its runs stay, so registering the same time again never refires it', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T10:00:00Z');
  const once = { id: 'w', agent: 'a', times: ['2026-01-01T10:00:00Z'], deadline: '2026-01-01T12:00:00Z', sessionKey: 'w', input: 'x' };
  await put(api, once);
  await svc.tick();
  assert.equal(calls.length, 1);
  assert.deepEqual((await api('GET', '/')).json.schedules, []);
  await put(api, once);
  clock = new Date('2026-01-01T11:00:00Z');
  await svc.tick();
  assert.equal(calls.length, 1);
  assert.deepEqual((await api('GET', '/')).json.schedules, []);
  // A one-shot still ahead, and a cron, are kept.
  await put(api, { ...once, id: 'later', times: ['2026-01-02T10:00:00Z'], deadline: '2026-01-02T12:00:00Z' });
  await put(api, daily);
  await svc.tick();
  assert.deepEqual((await api('GET', '/')).json.schedules.map((s: any) => s.id), ['daily', 'later']);
});

test('schedules and their history survive a restart', async () => {
  const path = await dbFile();
  const first = await start(path);
  clock = new Date('2026-01-01T08:00:00Z');
  await put(first.api, daily);
  clock = new Date('2026-01-01T09:00:00Z');
  await first.svc.tick();
  // Closes the first server too, which a bare svc.stop() left listening.
  open.pop()!.stop();

  clock = new Date('2026-01-01T09:05:00Z');
  const second = await start(path);
  await second.svc.tick();
  assert.equal(calls.length, 1, 'the recorded occurrence is not fired again');
  assert.equal((await second.api('GET', '/')).json.schedules[0].lastRun, '2026-01-01T09:00:00.000Z');
});

test('a cron in America/New_York keeps its wall-clock time across the DST change', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-03-07T12:00:00Z');
  await put(api, { ...daily, tz: 'America/New_York' });
  clock = new Date('2026-03-08T14:00:00Z');
  await svc.tick();
  clock = new Date('2026-03-09T13:00:00Z');
  await svc.tick();
  // Clocks spring forward on Mar 8: 09:00 EST was 14:00Z, 09:00 EDT is 13:00Z (UTC would drift an hour).
  assert.deepEqual(calls.map((c) => c.body.requestId), ['schedule:daily:2026-03-08T13:00:00.000Z', 'schedule:daily:2026-03-09T13:00:00.000Z']);
  assert.equal((await put(api, { ...daily, id: 'z', tz: 'Mars/Base' })).status, 400);
});

test('an exhausted one-shot whose moment passed long ago is recorded missed, not fired', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T00:00:00Z');
  await put(api, { id: 'once', agent: 'a', times: ['2026-01-01T01:00:00Z'], sessionKey: 'once', input: 'x' });
  clock = new Date('2026-01-02T01:00:00Z');
  await svc.tick();
  assert.equal(calls.length, 0);
  assert.deepEqual((await api('GET', '/')).json.schedules, [], 'recorded missed, it is spent and deleted');
});

test('a one-shot registered for the moment it is written fires once, and re-registering it does not fire it again', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T10:00:00Z');
  const once = { id: 'now', agent: 'a', times: ['2026-01-01T10:00:00Z'], sessionKey: 'now', input: 'x' };
  await put(api, once);
  await svc.tick();
  assert.equal(calls.length, 1);
  await put(api, { ...once, input: 'y' });
  await svc.tick();
  assert.equal(calls.length, 1);
});

test('a one-shot registered after its time but inside its window fires; a recorded missed one is not retried', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T10:30:00Z');
  const late = { id: 'late', agent: 'a', times: ['2026-01-01T10:00:00Z'], deadline: '2026-01-01T12:00:00Z', sessionKey: 'late', input: 'x' };
  await put(api, late);
  await svc.tick();
  assert.equal(calls.length, 1);
  await put(api, { id: 'gone', agent: 'a', times: ['2026-01-01T09:00:00Z'], deadline: '2026-01-01T09:30:00Z', sessionKey: 'gone', input: 'x' });
  await svc.tick();
  await put(api, { id: 'gone', agent: 'a', times: ['2026-01-01T09:00:00Z'], deadline: '2026-01-01T11:00:00Z', sessionKey: 'gone', input: 'x' });
  await svc.tick();
  assert.equal(calls.length, 1, 'past its window it was recorded missed, and a wider window later does not revive it');
});

test('a tick that throws is logged and the loop keeps running', async () => {
  const { svc, api } = await start(':memory:', 1);
  clock = new Date('2026-01-01T08:00:00Z');
  await put(api, daily);
  clock = new Date('2026-01-01T09:00:00Z');
  const fired = new Promise<void>((r) => (onFetch = r));
  failClockOnce = true;
  await svc.start?.();
  await fired;
  assert.equal(calls.length, 1);
});

test('a schedule can be deleted: 204 with no body, then 404', async () => {
  const { api } = await start(':memory:');
  await put(api, daily);
  const gone = await api('DELETE', '/daily');
  assert.deepEqual([gone.status, gone.text], [204, '']);
  assert.deepEqual((await api('GET', '/')).json.schedules, []);
  const again = await api('DELETE', '/daily');
  assert.deepEqual([again.status, again.type, again.json.status], [404, 'application/problem+json', 404]);
});

test('PUT creates with 201 then replaces with 200, answering the view without source', async () => {
  const { api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  const created = await put(api, daily);
  assert.equal(created.status, 201);
  assert.deepEqual(created.json, { ...daily, lastRun: null, nextRun: '2026-01-01T09:00:00.000Z', missedRuns: [] });
  const replaced = await put(api, { ...daily, input: 'again' });
  assert.equal(replaced.status, 200);
  assert.equal(replaced.json.input, 'again');
  assert.equal((await api('GET', '/')).json.schedules[0].source, undefined);
});

test('an id with a slash travels as one percent-encoded segment', async () => {
  const { api } = await start(':memory:');
  const id = 'sleeper/lineup/1/2/lineup';
  assert.equal((await put(api, { ...daily, id })).status, 201);
  assert.equal((await api('GET', '/')).json.schedules[0].id, id);
  assert.equal((await api('DELETE', `/${encodeURIComponent(id)}`)).status, 204);
  assert.equal((await api('DELETE', '/sleeper/lineup')).status, 404, 'unencoded slashes are an unknown path');
});

test('the body is closed: an id or any unknown field is 400, as problem+json', async () => {
  const { id, ...def } = daily;
  const { api } = await start(':memory:');
  for (const extra of [{ id: 'daily' }, { colour: 'red' }]) {
    const res = await api('PUT', '/daily', { ...def, ...extra });
    assert.deepEqual([res.status, res.type], [400, 'application/problem+json']);
    assert.match(res.json.detail, /unknown field/);
  }
  assert.deepEqual((await api('GET', '/')).json.schedules, []);
});

test('an error is problem+json with type, title, status, detail and error', async () => {
  const { api } = await start(':memory:');
  const res = await api('PUT', '/daily', { agent: 'a' });
  assert.equal(res.type, 'application/problem+json');
  assert.deepEqual(res.json, { type: 'about:blank', title: 'Bad Request', status: 400, detail: res.json.detail, error: res.json.detail });
});

test('a wrong method is 405 with Allow, an unknown path a 404 problem', async () => {
  const { api } = await start(':memory:');
  const post = await api('POST', '/', daily);
  assert.deepEqual([post.status, post.allow, post.type], [405, 'GET', 'application/problem+json']);
  assert.deepEqual([(await api('GET', '/daily')).status, (await api('GET', '/daily')).allow], [405, 'DELETE, PUT']);
  assert.equal((await api('PUT', '/')).status, 405);
  const unknown = await api('GET', '/a/b');
  assert.deepEqual([unknown.status, unknown.type], [404, 'application/problem+json']);
});

test('the list pages by id: pageSize, nextPageToken only until the last page, and bad inputs are 400', async () => {
  const { api } = await start(':memory:');
  for (const id of ['c', 'a', 'd', 'b', 'e']) await put(api, { ...daily, id });
  const first = (await api('GET', '/?pageSize=2')).json;
  assert.deepEqual(first.schedules.map((s: any) => s.id), ['a', 'b']);
  const second = (await api('GET', `/?pageSize=2&pageToken=${first.nextPageToken}`)).json;
  assert.deepEqual(second.schedules.map((s: any) => s.id), ['c', 'd']);
  const last = (await api('GET', `/?pageSize=2&pageToken=${second.nextPageToken}`)).json;
  assert.deepEqual([last.schedules.map((s: any) => s.id), 'nextPageToken' in last], [['e'], false]);
  const whole = (await api('GET', '/?pageSize=5')).json;
  assert.equal('nextPageToken' in whole, false, 'a page that ends exactly at the end has no token');
  assert.equal((await api('GET', '/')).json.schedules.length, 5, 'the default size is 100');
  const foreign = Buffer.from(JSON.stringify(['sessions', 'a'])).toString('base64url');
  for (const q of ['pageToken=nope', `pageToken=${foreign}`, 'pageSize=0', 'pageSize=1001', 'pageSize=x']) {
    const res = await api('GET', `/?${q}`);
    assert.deepEqual([res.status, res.type], [400, 'application/problem+json'], q);
  }
});

test('the API rejects bad definitions and an oversized body', async () => {
  const { api, base } = await start(':memory:');
  assert.equal((await put(api, { ...daily, cron: 'nope' })).status, 400);
  assert.equal((await put(api, { ...daily, times: ['2026-01-01T00:00:00Z'] })).status, 400);
  const big = await fetch(`${base}/daily`, { method: 'PUT', body: 'x'.repeat(2 << 20) });
  assert.deepEqual([big.status, big.headers.get('content-type')], [413, 'application/problem+json']);
});

test('the retention sweep deletes only sessions idle past their agent retention, read from the agent', async () => {
  const sessions = [
    { key: 'old', agent: 'reviewer', state: 'stopped', pendingAsks: 0, lastActivity: '2026-01-01T00:00:00Z' },
    { key: 'recent', agent: 'reviewer', state: 'idle', pendingAsks: 0, lastActivity: '2026-01-20T00:00:00Z' },
    { key: 'keep-forever', agent: 'researcher', state: 'stopped', pendingAsks: 0, lastActivity: '2025-01-01T00:00:00Z' },
    { key: 'home:main', agent: 'reviewer', state: 'stopped', pendingAsks: 0, lastActivity: '2025-01-01T00:00:00Z' },
    { key: 'running', agent: 'reviewer', state: 'running', pendingAsks: 0, lastActivity: '2026-01-01T00:00:00Z' },
  ];
  const deletes: string[] = [];
  const asked: string[] = [];
  const agentsRead: string[] = [];
  const definitions: Record<string, unknown> = { reviewer: { retention: { idleDays: 14 } }, researcher: {} };
  const fetchStub = (async (url: string, init?: { method?: string }) => {
    const agent = /\/agents\/([^/?]+)$/.exec(url);
    if (agent) {
      agentsRead.push(decodeURIComponent(agent[1]!));
      return { ok: true, json: async () => ({ definition: definitions[decodeURIComponent(agent[1]!)] }) };
    }
    if (url.endsWith('/agents')) return { ok: true, json: async () => ({ agents: [] }) };
    if (init?.method !== 'DELETE') {
      // Two rows to a page, running sessions filtered out as the q asks.
      const q = new URL(url).searchParams;
      asked.push(q.get('q') ?? '');
      const rows = sessions.filter((s) => s.state !== 'running');
      const from = Number(q.get('pageToken') ?? 0);
      return { ok: true, json: async () => ({ sessions: rows.slice(from, from + 2), ...(from + 2 < rows.length && { nextPageToken: String(from + 2) }) }) };
    }
    deletes.push(decodeURIComponent(url.split('/sessions/')[1]!));
    return { ok: true, status: 200 };
  }) as unknown as typeof fetch;
  clock = new Date('2026-01-25T00:00:00Z');
  const svc = createSchedulesService({ distributorUrl: 'http://d', dbPath: ':memory:', distributorToken: 'admin', retentionSweep: true, now: () => clock, fetch: fetchStub });
  open.push(svc);
  await svc.tick();
  assert.deepEqual(deletes, ['old']);
  assert.deepEqual(asked, ['-state:running AND pendingAsks:0', '-state:running AND pendingAsks:0'], 'the four sessions took two pages');
  assert.deepEqual(agentsRead.sort(), ['researcher', 'reviewer'], 'each agent read once');
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
  const res = await put(api, noAgent);
  assert.equal(res.status, 400);
  assert.match(res.json.detail, /agent/);
  assert.deepEqual((await api('GET', '/')).json.schedules, []);
});

