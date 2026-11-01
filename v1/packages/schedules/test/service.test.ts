import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, test } from 'node:test';
import { createSchedulesService } from '../src/index.ts';

const TOKEN = 't';
const auth = { authorization: `Bearer ${TOKEN}` };
type Call = { url: string; body: { input: string; requestId: string } };

let clock = new Date('2026-01-01T00:00:00Z');
let calls: Call[] = [];
let ok = true;
const open: Array<ReturnType<typeof createSchedulesService>> = [];
afterEach(() => {
  open.splice(0).forEach((s) => s.stop());
  calls = [];
  ok = true;
});

const fakeFetch = (async (url: string, init: { body: string }) => {
  calls.push({ url, body: JSON.parse(init.body) });
  return { ok };
}) as unknown as typeof fetch;

async function start(dbPath: string, agentsDir?: string) {
  const svc = createSchedulesService({ serviceToken: TOKEN, distributorUrl: 'http://d', dbPath, agentsDir, now: () => clock, fetch: fakeFetch });
  open.push(svc);
  svc.listen(0);
  await new Promise((r) => svc.once('listening', r));
  const base = `http://127.0.0.1:${(svc.address() as AddressInfo).port}`;
  const api = (method: string, path: string, body?: unknown) =>
    fetch(base + path, { method, headers: auth, body: body ? JSON.stringify(body) : undefined }).then(async (r) => ({ status: r.status, json: (await r.json()) as any }));
  return { svc, api, base };
}

const daily = { id: 'daily', cron: '0 9 * * *', sessionKey: 'daily-{time}', input: 'go' };
const dbFile = async () => join(await mkdtemp(join(tmpdir(), 'sched-')), 's.db');

test('fires at the occurrence with a deterministic request id, once', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  assert.equal((await api('POST', '/schedules', daily)).status, 201);
  await svc.tick();
  assert.equal(calls.length, 0);
  clock = new Date('2026-01-01T09:00:00Z');
  await svc.tick();
  await svc.tick();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, `http://d/sessions/${encodeURIComponent('daily-2026-01-01T09:00:00.000Z')}/invocations`);
  assert.deepEqual(calls[0].body, { input: 'go', requestId: 'schedule:daily:2026-01-01T09:00:00.000Z' });
  const [listed] = (await api('GET', '/schedules')).json.schedules;
  assert.equal(listed.lastRun, '2026-01-01T09:00:00.000Z');
  assert.equal(listed.nextRun, '2026-01-02T09:00:00.000Z');
});

test('a failed submission is retried, not recorded', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  await api('POST', '/schedules', daily);
  clock = new Date('2026-01-01T09:00:00Z');
  ok = false;
  await svc.tick();
  ok = true;
  await svc.tick();
  assert.equal(calls.length, 2);
  assert.equal(calls[0].body.requestId, calls[1].body.requestId);
  assert.equal((await api('GET', '/schedules')).json.schedules[0].lastRun, '2026-01-01T09:00:00.000Z');
});

test('after downtime only the newest useful miss fires and the rest are recorded missed', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  await api('POST', '/schedules', { ...daily, staleMs: 6 * 3600_000 });
  clock = new Date('2026-01-04T10:00:00Z');
  await svc.tick();
  assert.deepEqual(calls.map((c) => c.body.requestId), ['schedule:daily:2026-01-04T09:00:00.000Z']);
  const [listed] = (await api('GET', '/schedules')).json.schedules;
  assert.deepEqual(listed.missedRuns, ['2026-01-01T09:00:00.000Z', '2026-01-02T09:00:00.000Z', '2026-01-03T09:00:00.000Z']);
});

test('a newest miss past its window is recorded missed, not fired', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T08:00:00Z');
  await api('POST', '/schedules', { ...daily, staleMs: 3600_000 });
  clock = new Date('2026-01-02T12:00:00Z');
  await svc.tick();
  assert.equal(calls.length, 0);
  assert.deepEqual((await api('GET', '/schedules')).json.schedules[0].missedRuns, ['2026-01-01T09:00:00.000Z', '2026-01-02T09:00:00.000Z']);
});

test('the default window is the next occurrence, and explicit times honor a deadline', async () => {
  const { svc, api } = await start(':memory:');
  clock = new Date('2026-01-01T00:00:00Z');
  await api('POST', '/schedules', { id: 'once', times: ['2026-01-01T01:00:00Z'], sessionKey: 'once', input: 'x', deadline: '2026-01-01T01:30:00Z' });
  await api('POST', '/schedules', { id: 'two', times: ['2026-01-01T01:00:00Z', '2026-01-01T02:00:00Z'], sessionKey: 'two', input: 'x' });
  clock = new Date('2026-01-01T02:00:00Z');
  await svc.tick();
  assert.deepEqual(calls.map((c) => c.body.requestId), ['schedule:two:2026-01-01T02:00:00.000Z']);
  const byId = Object.fromEntries((await api('GET', '/schedules')).json.schedules.map((s: any) => [s.id, s]));
  assert.deepEqual(byId.once.missedRuns, ['2026-01-01T01:00:00.000Z']);
  assert.deepEqual(byId.two.missedRuns, ['2026-01-01T01:00:00.000Z']);
});

test('schedules and their history survive a restart', async () => {
  const path = await dbFile();
  const first = await start(path);
  clock = new Date('2026-01-01T08:00:00Z');
  await first.api('POST', '/schedules', daily);
  clock = new Date('2026-01-01T09:00:00Z');
  await first.svc.tick();
  first.svc.stop();
  open.length = 0;

  clock = new Date('2026-01-01T09:05:00Z');
  const second = await start(path);
  await second.svc.tick();
  assert.equal(calls.length, 1, 'the recorded occurrence is not fired again');
  assert.equal((await second.api('GET', '/schedules')).json.schedules[0].lastRun, '2026-01-01T09:00:00.000Z');
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
  assert.equal((await api('DELETE', '/schedules/sleeper%2Fweekly')).status, 404);
  await def([]);
  await svc.tick();
  assert.deepEqual((await api('GET', '/schedules')).json.schedules, []);
});

test('the API requires the token and rejects bad definitions', async () => {
  const { api, base } = await start(':memory:');
  assert.equal((await fetch(`${base}/schedules`)).status, 401);
  assert.equal((await api('POST', '/schedules', { ...daily, cron: 'nope' })).status, 400);
  assert.equal((await api('POST', '/schedules', { ...daily, times: ['2026-01-01T00:00:00Z'] })).status, 400);
  assert.equal((await api('DELETE', '/schedules/daily')).status, 404);
});
