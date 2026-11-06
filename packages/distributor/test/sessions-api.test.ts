import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { AddressInfo } from 'node:net';
import { DatabaseSync } from 'node:sqlite';
import { after, test } from 'node:test';
import type { Fact } from '@quack/index';
import { createDistributor, processDriver, recordTurns, sessionDirName, type HostDriver } from '../src/index.ts';
import { PERSON, PROXY_SECRET } from './fixtures/proxy.ts';

const cleanups: Array<() => void> = [];
after(() => cleanups.forEach((fn) => fn()));

type Seed = { agent?: string; activity?: number; archived?: boolean; title?: string };
type Row = { key: string; agent: string; lastActivity: string; running: boolean; active?: boolean; title?: string; archived?: boolean; cost?: number; tokens?: object };

const SESSIONS: Record<string, Seed> = {
  'sleeper:platinum:2:lineup': { agent: 'sleeper', activity: 5000, title: 'Week 2' },
  'sleeper:platinum:3:waivers': { agent: 'sleeper', activity: 6000 },
  'sleeper:gold:1:lineup': { agent: 'sleeper', activity: 4000 },
  'contracts:abc': { agent: 'contracts', activity: 3000, archived: true },
  'github:o/r#1': { agent: 'reviewer', activity: 6000 },
};

async function seed(dataDir: string, key: string, s: Seed) {
  const dir = join(dataDir, sessionDirName(key));
  await mkdir(dir, { recursive: true });
  await writeFile(join(dir, 'session-key'), key);
  await writeFile(join(dir, 'agent'), s.agent ?? 'a');
  await writeFile(join(dir, 'created'), '1000');
  if (s.activity) await writeFile(join(dir, 'activity'), String(s.activity));
  if (s.archived) await writeFile(join(dir, 'archived'), '');
  if (s.title) await writeFile(join(dir, 'title'), s.title);
  return dir;
}

// A real driver over seeded session directories; a read that tried to start a process would throw.
async function start(opts: { sessions?: Record<string, Seed>; running?: string[]; token?: string; /** Where a running session answers /ping; absent, any process call throws. */ pingUrl?: string; spend?: () => Promise<Array<{ key: string; cost: number; tokens: { input: number; output: number; cacheRead: number; cacheWrite: number } }>> } = {}) {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-api-'));
  cleanups.push(() => void rm(dataDir, { recursive: true, force: true }));
  for (const [key, s] of Object.entries(opts.sessions ?? SESSIONS)) await seed(dataDir, key, s);
  const real = processDriver({ command: [], dataDir });
  const running = opts.running ?? [];
  const stopped: string[] = [];
  const stop = async (k: string) => void (stopped.push(k), running.splice(running.indexOf(k), 1));
  const ensure = async () => { if (opts.pingUrl) return opts.pingUrl; throw new Error('a read must not start a process'); };
  const driver: HostDriver = { ...real, running: () => running, stop, sessions: async () => (await real.sessions()).map((s) => ({ ...s, running: running.includes(s.key) })), session: async (k) => { const s = await real.session(k); return s && { ...s, running: running.includes(s.key) }; }, ensure, setWake: async () => {} };
  const forgotten: string[] = [];
  const server = createDistributor(driver, { proxySecret: PROXY_SECRET, retire: { token: opts.token, waiting: async () => false, forget: async (k) => void forgotten.push(k) }, spend: opts.spend }).listen(0);
  await new Promise((r) => server.once('listening', r));
  cleanups.push(() => server.close());
  const url = (path: string) => `http://127.0.0.1:${(server.address() as AddressInfo).port}${path}`;
  const call = async (method: string, path: string, init: { body?: unknown; headers?: Record<string, string> } = {}) => {
    // A browser call arrives through the proxy with the caller's identity; a test passes headers to be another caller.
    const res = await fetch(url(path), { method, headers: init.headers ?? { ...PERSON, ...(init.body ? { 'content-type': 'application/json' } : {}) }, body: init.body === undefined ? undefined : JSON.stringify(init.body) });
    return { status: res.status, type: res.headers.get('content-type'), body: (await res.json()) as any };
  };
  const list = (query = '') => call('GET', `/sessions${query}`);
  const keys = async (filter: string) => (await list(`?filter=${encodeURIComponent(filter)}`)).body.sessions.map((s: Row) => s.key).sort();
  return { call, list, keys, dataDir, forgotten, driver, stopped };
}

const dirOf = (dataDir: string, key: string) => join(dataDir, sessionDirName(key));
const sorted = (xs: string[]) => [...xs].sort();

test('filter terms select by key, key prefix, agent, running and archived, and negate with a minus', async () => {
  const { keys } = await start({ running: ['github:o/r#1'] });
  assert.deepEqual(await keys('key:contracts:abc'), ['contracts:abc']);
  assert.deepEqual(await keys('key:sleeper:platinum:*'), ['sleeper:platinum:2:lineup', 'sleeper:platinum:3:waivers']);
  assert.deepEqual(await keys('agent:reviewer'), ['github:o/r#1']);
  assert.deepEqual(await keys('running:true'), ['github:o/r#1']);
  assert.equal((await keys('running:false')).length, 4);
  assert.deepEqual(await keys('archived:true'), ['contracts:abc']);
  assert.equal((await keys('archived:false')).length, 4);
  assert.deepEqual(await keys('-archived:true AND key:sleeper:*'), sorted(['sleeper:platinum:2:lineup', 'sleeper:platinum:3:waivers', 'sleeper:gold:1:lineup']));
  assert.deepEqual(await keys('-key:sleeper:* AND -agent:reviewer'), ['contracts:abc']);
  assert.equal((await keys('')).length, 5);
});

test('filter values may be quoted; a quoted star is literal and a quoted value may hold AND', async () => {
  const { keys } = await start();
  assert.deepEqual(await keys('key:"github:o/r#1"'), ['github:o/r#1']);
  assert.deepEqual(await keys('agent:"sleeper" AND key:"sleeper:gold:1:lineup"'), ['sleeper:gold:1:lineup']);
  assert.deepEqual(await keys('key:"sleeper:*"'), []);
  assert.deepEqual(await keys('agent:"no AND such"'), []);
  assert.deepEqual(await keys('agent:"say \\"hi\\""'), []);
});

test('a filter the backend cannot parse, or a field it does not support, is a 400 problem', async () => {
  const { list } = await start();
  const bad = ['title:x', 'key:a OR key:b', 'NOT key:a', 'key:a key:b', 'key:a and key:b', 'agent:sleeper*', 'running:maybe', 'running:true*', 'running:"true"', 'key:a*b', 'key:', 'key', '(key:a)', 'key:a AND', 'AND key:a', '--key:a', 'KEY:a', 'key:[a TO b]', 'key:"unterminated'];
  for (const filter of bad) {
    const res = await list(`?filter=${encodeURIComponent(filter)}`);
    assert.deepEqual([filter, res.status, res.type], [filter, 400, 'application/problem+json']);
    assert.match(res.body.detail, /^filter:/);
  }
});

test('pages come newest activity first with key ascending on ties, and the token resumes strictly after the last row', async () => {
  const { list } = await start();
  const order = ['github:o/r#1', 'sleeper:platinum:3:waivers', 'sleeper:platinum:2:lineup', 'sleeper:gold:1:lineup', 'contracts:abc'];
  assert.deepEqual((await list()).body.sessions.map((s: Row) => s.key), order);
  const seen: string[] = [];
  let token = '';
  let pages = 0;
  do {
    const { status, body } = await list(`?pageSize=2${token && `&pageToken=${token}`}`);
    assert.equal(status, 200);
    assert.ok(body.sessions.length <= 2);
    seen.push(...body.sessions.map((s: Row) => s.key));
    token = body.nextPageToken ?? '';
    pages++;
  } while (token);
  assert.deepEqual([seen, pages], [order, 3]);
  const last = await list(`?pageSize=5`);
  assert.equal(last.body.nextPageToken, undefined, 'an exact fit leaves no further page');
});

test('a page filters before it pages, and a row that appears between pages does not repeat or skip', async () => {
  const { list, dataDir } = await start();
  const first = await list(`?pageSize=1&filter=${encodeURIComponent('key:sleeper:*')}`);
  assert.deepEqual(first.body.sessions.map((s: Row) => s.key), ['sleeper:platinum:3:waivers']);
  await seed(dataDir, 'sleeper:platinum:9:newest', { agent: 'sleeper', activity: 9000 });
  const second = await list(`?pageSize=5&filter=${encodeURIComponent('key:sleeper:*')}&pageToken=${first.body.nextPageToken}`);
  assert.deepEqual(second.body.sessions.map((s: Row) => s.key), ['sleeper:platinum:2:lineup', 'sleeper:gold:1:lineup']);
});

test('a pageSize above 1000 is capped; a pageSize that is not a positive integer, or a token this server did not issue, is 400', async () => {
  const { list } = await start();
  assert.equal((await list('?pageSize=5000')).status, 200);
  const junk = [`?pageSize=0`, `?pageSize=-1`, `?pageSize=abc`, `?pageSize=1.5`, `?pageToken=nope`, `?pageToken=${Buffer.from('[1]').toString('base64url')}`, `?pageToken=${Buffer.from('["x","k"]').toString('base64url')}`, `?pageToken=${Buffer.from(' [1,"k"]').toString('base64url')}`];
  for (const q of junk) assert.equal((await list(q)).status, 400, q);
});

test('rows carry stored metadata and spend from the index, which is optional', async () => {
  const tokens = { input: 1, output: 2, cacheRead: 3, cacheWrite: 4 };
  const withIndex = await start({ spend: async () => [{ key: 'sleeper:gold:1:lineup', cost: 1.5, tokens }] });
  const rows: Row[] = (await withIndex.list()).body.sessions;
  const gold = rows.find((s) => s.key === 'sleeper:gold:1:lineup')!;
  assert.deepEqual([gold.cost, gold.tokens, gold.agent, gold.lastActivity], [1.5, tokens, 'sleeper', new Date(4000).toISOString()]);
  assert.equal(rows.find((s) => s.key === 'contracts:abc')!.cost, undefined, 'a session the index does not know has no spend');
  assert.equal(rows.find((s) => s.key === 'contracts:abc')!.archived, true);
  const down = await start({ spend: async () => { throw new Error('index down'); } });
  const plain: Row[] = (await down.list()).body.sessions;
  assert.deepEqual([plain.length, plain.some((s) => 'cost' in s || 'tokens' in s)], [5, false]);
});

test('GET one returns the row, 404s an unknown key and 400s an invalid one, without starting a process', async () => {
  const { call } = await start({ spend: async () => [{ key: 'sleeper:platinum:2:lineup', cost: 2, tokens: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }] });
  const one = await call('GET', `/sessions/${encodeURIComponent('sleeper:platinum:2:lineup')}`);
  assert.deepEqual([one.status, one.body.key, one.body.title, one.body.cost, one.body.running], [200, 'sleeper:platinum:2:lineup', 'Week 2', 2, false]);
  const missing = await call('GET', '/sessions/research%3Aghost');
  assert.deepEqual([missing.status, missing.type], [404, 'application/problem+json']);
  assert.equal((await call('GET', '/sessions/no%20spaces')).status, 400);
  assert.equal((await call('GET', '/sessions/%ZZ')).status, 400);
});

test('PATCH answers the updated session and rejects a bad title or flag', async () => {
  const { call } = await start();
  const key = encodeURIComponent('sleeper:gold:1:lineup');
  const renamed = await call('PATCH', `/sessions/${key}`, { body: { title: ' Gold ' } });
  assert.deepEqual([renamed.status, renamed.body.key, renamed.body.title], [200, 'sleeper:gold:1:lineup', 'Gold']);
  const archived = await call('PATCH', `/sessions/${key}`, { body: { archived: true } });
  assert.deepEqual([archived.body.archived, archived.body.title], [true, 'Gold']);
  const restored = await call('PATCH', `/sessions/${key}`, { body: { title: '  ', archived: false } });
  assert.deepEqual([restored.body.archived, restored.body.title], [undefined, undefined]);
  assert.equal((await call('PATCH', `/sessions/${key}`, { body: { title: 'x'.repeat(121) } })).status, 400);
  assert.equal((await call('PATCH', `/sessions/${key}`, { body: { archived: 'yes' } })).status, 400);
});

test('DELETE removes the session, is refused while it runs, and a second delete is 404 with GET agreeing', async () => {
  const { call, forgotten, list } = await start({ running: ['github:o/r#1'] });
  const key = (k: string) => `/sessions/${encodeURIComponent(k)}`;
  assert.equal((await call('DELETE', key('github:o/r#1'))).status, 409);
  const done = await call('DELETE', key('contracts:abc'));
  assert.deepEqual([done.status, done.body], [200, { key: 'contracts:abc' }]);
  assert.equal((await call('GET', key('contracts:abc'))).status, 404);
  assert.equal((await call('DELETE', key('contracts:abc'))).status, 404);
  assert.deepEqual(forgotten, ['contracts:abc']);
  assert.equal((await list()).body.sessions.length, 4);
  assert.equal((await call('DELETE', '/sessions/no%20spaces')).status, 400);
});

test('DELETE refuses a session mid-run, and stops then deletes one whose process is only idle or watched', async () => {
  let status = 'HealthyBusy';
  const proc = createServer((_req, res) => res.end(JSON.stringify({ status }))).listen(0);
  await new Promise((r) => proc.once('listening', r));
  cleanups.push(() => proc.close());
  const { call, stopped } = await start({ running: ['github:o/r#1'], pingUrl: `http://127.0.0.1:${(proc.address() as AddressInfo).port}` });
  const key = `/sessions/${encodeURIComponent('github:o/r#1')}`;
  const busy = await call('DELETE', key);
  assert.deepEqual([busy.status, busy.body.detail], [409, 'session is running']);
  assert.deepEqual(stopped, []);
  status = 'HealthyIdle';
  assert.equal((await call('DELETE', key)).status, 200);
  assert.deepEqual(stopped, ['github:o/r#1']);
  assert.equal((await call('GET', key)).status, 404);
});

test('rename, archive and delete take the admin token or the proxy identity and refuse a wrong token or none; the legacy POST route takes only the identity', async () => {
  const { call, keys } = await start({ token: 'admin' });
  const key = (k: string) => `/sessions/${encodeURIComponent(k)}`;
  const bearer = (t: string) => ({ headers: { authorization: `Bearer ${t}`, 'content-type': 'application/json' } });
  const none = { headers: { 'content-type': 'application/json' } };
  assert.equal((await call('DELETE', key('contracts:abc'), bearer('wrong'))).status, 401);
  assert.equal((await call('DELETE', key('contracts:abc'), none)).status, 401);
  assert.equal((await call('PATCH', key('contracts:abc'), { ...none, body: { title: 'x' } })).status, 401);
  assert.equal((await call('PATCH', key('contracts:abc'), { ...bearer('admin'), body: { title: 'x' } })).status, 200);
  assert.equal((await call('POST', `${key('sleeper:platinum:2:lineup')}/delete`, { ...bearer('admin'), body: {} })).status, 401);
  assert.equal((await call('DELETE', key('contracts:abc'), bearer('admin'))).status, 200);
  assert.equal((await call('DELETE', key('sleeper:gold:1:lineup'), { headers: { ...PERSON } })).status, 200);
  const legacy = await call('POST', `${key('sleeper:platinum:2:lineup')}/delete`, { body: {} });
  assert.deepEqual([legacy.status, legacy.body], [200, { key: 'sleeper:platinum:2:lineup' }]);
  assert.equal((await call('POST', `${key('sleeper:platinum:2:lineup')}/delete`, { body: {} })).status, 404);
  assert.deepEqual(await keys(''), ['github:o/r#1', 'sleeper:platinum:3:waivers']);
});

test('listing reads the stored last activity: a store replaced by junk changes nothing', async () => {
  const { dataDir, list } = await start();
  const before = (await list()).body.sessions;
  for (const key of Object.keys(SESSIONS)) await writeFile(join(dirOf(dataDir, key), 'session.sqlite'), 'not a database');
  assert.deepEqual((await list()).body.sessions, before);
});

test('a session with no stored last activity is backfilled once from its log, then never read again', async () => {
  const { dataDir, list, call } = await start({ sessions: { legacy: { agent: 'a' } } });
  const dir = dirOf(dataDir, 'legacy');
  const db = new DatabaseSync(join(dir, 'session.sqlite'));
  db.exec('CREATE TABLE entries (record TEXT)');
  for (const at of [Date.parse('2026-11-01T00:00:00Z'), Date.parse('2026-11-03T00:00:00Z'), Date.parse('2026-11-02T00:00:00Z')]) db.prepare('INSERT INTO entries VALUES (?)').run(JSON.stringify({ model: [{ timestamp: at }] }));
  db.close();
  await writeFile(join(dir, 'touched'), String(Date.parse('2026-11-02T12:00:00Z')));
  assert.equal((await list()).body.sessions[0].lastActivity, '2026-11-03T00:00:00.000Z');
  assert.equal(await readFile(join(dir, 'activity'), 'utf8'), String(Date.parse('2026-11-03T00:00:00Z')));
  await writeFile(join(dir, 'session.sqlite'), 'not a database');
  assert.equal((await call('GET', '/sessions/legacy')).body.lastActivity, '2026-11-03T00:00:00.000Z');
});

test('a legacy session with an empty log falls back to its creation time', async () => {
  const { list } = await start({ sessions: { empty: {} } });
  assert.equal((await list()).body.sessions[0].lastActivity, new Date(1000).toISOString());
});

const idle = (at: number): Fact => ({ id: 'status', kind: 'status', rev: 2, at, data: { state: 'idle' } });

test('a completed turn (status turning idle) stores its end time as the last activity, and never moves it back', async () => {
  const { driver, call, dataDir } = await start();
  const turns = recordTurns(driver);
  const key = 'sleeper:gold:1:lineup';
  const read = async () => (await call('GET', `/sessions/${encodeURIComponent(key)}`)).body.lastActivity;
  await turns(key, [{ id: 'status', kind: 'status', rev: 1, at: 9000, data: { state: 'running' } }]);
  assert.equal(await read(), new Date(4000).toISOString(), 'a run starting is not a completed turn');
  await turns(key, [{ id: 'cost', kind: 'cost', rev: 1, at: 9000, data: {} }]);
  assert.equal(await read(), new Date(4000).toISOString());
  await turns(key, [idle(8000)]);
  assert.equal(await read(), new Date(8000).toISOString());
  await turns(key, [idle(7000)]);
  assert.equal(await read(), new Date(8000).toISOString(), 'a resent older fact does not regress it');
  for (const k of Object.keys(SESSIONS)) await writeFile(join(dirOf(dataDir, k), 'session.sqlite'), 'junk');
  assert.equal(await read(), new Date(8000).toISOString());
});

test('a run that ended while the distributor was down is reflected when its session resends the status fact', async () => {
  const { driver, call, dataDir } = await start({ sessions: { down: { activity: 2000 } } });
  const ended = Date.parse('2026-11-04T10:00:00Z');
  await recordTurns(driver)('down', [idle(ended)]);
  assert.equal((await call('GET', '/sessions/down')).body.lastActivity, '2026-11-04T10:00:00.000Z');
  assert.equal(await readFile(join(dirOf(dataDir, 'down'), 'activity'), 'utf8'), String(ended));
});

test('a status fact for a deleted session is dropped without recreating its directory', async () => {
  const { driver, call, dataDir } = await start({ sessions: { gone: { activity: 1 } } });
  await call('DELETE', '/sessions/gone');
  await recordTurns(driver)('gone', [idle(5000)]);
  assert.equal((await call('GET', '/sessions/gone')).status, 404);
  await assert.rejects(readFile(join(dirOf(dataDir, 'gone'), 'activity')));
});
