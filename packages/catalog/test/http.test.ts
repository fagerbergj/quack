import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, before, test } from 'node:test';
import { escapeTerm } from '@quack/sdk';
import { createCatalog, fileJournal, sessionsApi, sqliteCatalog, type Caller, type CatalogEvent, type SessionsApiDeps } from '../src/index.ts';

const T0 = Date.parse('2026-11-01T00:00:00Z');
const cleanups: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

const deps = { refuse: undefined as string | undefined, answered: [] as unknown[], sessionSays: true };
let base = '';
let catalog: ReturnType<typeof createCatalog>;

const ev = (seq: number, type: string, data: unknown, id = `${type}:${seq}`) => ({ seq, id, at: T0 + seq * 1000, type, data }) as CatalogEvent;

before(async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-sessions-api-'));
  const store = sqliteCatalog(join(dir, 'c.sqlite'), { currency: 'USD' });
  let n = 0;
  catalog = createCatalog({ store, journal: fileJournal(join(dir, 'journal')), now: () => T0 + 99_000, newId: () => `r${++n}`, log: () => {} });
  const api: SessionsApiDeps = {
    deleteSession: async () => (deps.refuse ? { refused: deps.refuse } : 'deleted'),
    answer: async (_key, _ask, r) => { deps.answered.push(r); return deps.sessionSays; },
  };
  const route = sessionsApi(catalog, api);
  // The listener in front names the caller; tests pass it in a header.
  const server = createServer(async (req, res) => {
    const caller = JSON.parse(String(req.headers['x-caller'] ?? '{"kind":"person","id":"jason"}')) as Caller;
    if (!(await route(req, res, caller))) res.writeHead(418).end();
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  cleanups.push(async () => { await new Promise((r) => server.close(r)); await store.close(); await rm(dir, { recursive: true, force: true }); });
  for (const [i, key] of ['sleeper:platinum:1', 'sleeper:platinum:2', 'review:monorepo'].entries()) {
    await catalog.publish(key, [
      ev(1, 'session.created', { agent: 'quack', createdAt: T0 + i }), ev(2, 'title.set', { title: `Title ${i}` }),
      ev(3, 'spend.recorded', { cost: { currencyCode: 'USD', amountMicros: i === 1 ? 9007199254740993n : 1_250_000n }, tokens: { input: 1, output: 2, cacheRead: 0, cacheWrite: 0 } }),
      ev(4, 'artifact.sealed', { kind: 'sleeper.cards', version: 1, hash: 'h', content: `body ${i}`, summary: 'Matchday lineup' }),
      ev(5, 'artifact.delivered', { kind: 'sleeper.cards', version: 1, where: 'sleeper.board', link: 'https://x/1', at: T0 + 6000 }),
    ]);
  }
  await catalog.publish('review:monorepo', [ev(6, 'run.started', { run: 1 }), ev(7, 'ask.opened', { id: 'a-1', type: 'approval', createdAt: T0 + 7000, toolCall: { tool: 'merge', args: { pr: 1 } } })]);
});

const call = async (method: string, path: string, opts: { body?: unknown; caller?: Caller; type?: string } = {}) => {
  const res = await fetch(base + path, {
    method, headers: { ...(opts.caller && { 'x-caller': JSON.stringify(opts.caller) }), ...(opts.body !== undefined && { 'content-type': opts.type ?? 'application/json' }) },
    ...(opts.body !== undefined && { body: typeof opts.body === 'string' ? opts.body : JSON.stringify(opts.body) }),
  });
  return { status: res.status, type: res.headers.get('content-type'), body: (await res.json()) as Record<string, unknown> & { detail?: string } };
};
const k = (key: string) => encodeURIComponent(key);

test('the session list is one keyset page of v8 rows: Money as a decimal string, RFC 3339 dates', async () => {
  const first = await call('GET', '/sessions?pageSize=2&orderBy=createdAt');
  assert.equal(first.status, 200);
  const rows = first.body.sessions as Array<Record<string, unknown>>;
  assert.deepEqual(rows.map((r) => r.key), ['review:monorepo', 'sleeper:platinum:2']);
  assert.deepEqual(rows[1], {
    key: 'sleeper:platinum:2', agent: 'quack', title: 'Title 1', state: 'idle', createdAt: '2026-11-01T00:00:00.001Z', lastActivity: '2026-11-01T00:00:00.001Z',
    cost: { currencyCode: 'USD', amountMicros: '9007199254740993' }, tokens: { input: 1, output: 2, cacheRead: 0, cacheWrite: 0 }, pendingAsks: 0,
  });
  const next = await call('GET', `/sessions?pageSize=2&orderBy=createdAt&pageToken=${first.body.nextPageToken}`);
  assert.deepEqual((next.body.sessions as Array<{ key: string }>).map((r) => r.key), ['sleeper:platinum:1']);
  const league = await call('GET', `/sessions?q=${encodeURIComponent(`key:${escapeTerm('sleeper:platinum:')}*`)}`);
  assert.deepEqual((league.body.sessions as Array<{ key: string }>).map((r) => r.key).sort(), ['sleeper:platinum:1', 'sleeper:platinum:2']);
  const exact = await call('GET', `/sessions?q=${encodeURIComponent('cost.amountMicros:9007199254740993')}`);
  assert.deepEqual((exact.body.sessions as Array<{ key: string }>).map((r) => r.key), ['sleeper:platinum:2']);
});

test('a bad q, pageSize, orderBy or pageToken is a 400 problem', async () => {
  const bad = await call('GET', `/sessions?q=${encodeURIComponent('key:a:b')}`);
  assert.deepEqual([bad.status, bad.type], [400, 'application/problem+json']);
  assert.match(String(bad.body.detail), /escape .* or quote/);
  for (const q of ['pageSize=0', 'pageSize=1001', 'pageSize=x', 'orderBy=sealedAt', 'pageToken=bogus', 'q=nope:1']) assert.equal((await call('GET', `/sessions?${q}`)).status, 400, q);
  const page = await call('GET', '/sessions?pageSize=1');
  assert.equal((await call('GET', `/sessions?pageSize=1&q=state:idle&pageToken=${page.body.nextPageToken}`)).status, 400, 'a token is bound to its q');
});

test('a session credential reaches its own key, and a cross-session list only with the grant; elsewhere it is not found', async () => {
  const own: Caller = { kind: 'session', key: 'review:monorepo', crossSession: false };
  assert.equal((await call('GET', `/sessions/${k('review:monorepo')}`, { caller: own })).status, 200);
  assert.equal((await call('GET', `/sessions/${k('review:monorepo')}/asks`, { caller: own })).status, 200);
  assert.equal((await call('GET', `/sessions/${k('sleeper:platinum:1')}`, { caller: own })).status, 404);
  for (const path of ['/sessions', '/asks', '/artifacts']) assert.equal((await call('GET', path, { caller: own })).status, 404, path);
  assert.equal((await call('GET', '/asks', { caller: { ...own, crossSession: true } })).status, 200);
  assert.equal((await call('PATCH', `/sessions/${k('review:monorepo')}`, { caller: own, body: { title: 'x' } })).status, 401, 'never a write');
  assert.equal((await call('GET', '/sessions/bad%20key')).status, 400);
  assert.equal((await call('GET', `/sessions/${k('nope:1')}`)).status, 404);
});

test('PATCH renames, archives and unarchives, and refuses exactly as the contract says', async () => {
  const path = `/sessions/${k('sleeper:platinum:1')}`;
  assert.equal((await call('PATCH', path, { body: { title: 'x' }, type: 'text/plain' })).status, 415);
  assert.equal((await call('PATCH', path, { body: { title: 'x'.repeat(121) } })).status, 400);
  assert.equal((await call('PATCH', path, { body: { state: 'idle' } })).status, 422);
  const renamed = await call('PATCH', path, { body: { title: '  Mine  ', state: 'archived' } });
  assert.deepEqual([renamed.status, renamed.body.title, renamed.body.state], [200, 'Mine', 'archived']);
  assert.equal((await call('PATCH', path, { body: { state: 'archived' } })).status, 200, 'a write that changes nothing is a 200 no-op');
  const restored = await call('PATCH', path, { body: { title: ' ', state: 'stopped' } });
  assert.deepEqual([restored.body.title, restored.body.state], ['Title 0', 'stopped']);
  const running = `/sessions/${k('review:monorepo')}`;
  const refused = await call('PATCH', running, { body: { title: 'Never', state: 'archived' } });
  assert.equal(refused.status, 409);
  assert.equal((await call('GET', running)).body.title, 'Title 2', 'a refused archive changes nothing');
  assert.equal((await call('PATCH', `/sessions/${k('nope:1')}`, { body: { title: 'x' } })).status, 404);
});

test('asks: the needs-you list, one ask, and a nested resolution filled from the credential', async () => {
  const pending = await call('GET', '/asks?q=state:pending');
  assert.deepEqual((pending.body.asks as Array<Record<string, unknown>>).map((a) => [a.session, a.id, a.createdAt]), [['review:monorepo', 'a-1', '2026-11-01T00:00:07.000Z']]);
  const path = `/sessions/${k('review:monorepo')}/asks/a-1`;
  assert.equal((await call('GET', path)).body.state, 'pending');
  assert.equal((await call('PATCH', path, { body: { choice: 'approve' } })).status, 400, 'the resolution is nested');
  assert.equal((await call('PATCH', path, { body: { resolution: { choice: 'approve', by: { kind: 'person', id: 'mallory' } } } })).status, 400, 'by comes from the credential');
  deps.sessionSays = false;
  assert.equal((await call('PATCH', path, { body: { resolution: { choice: 'approve' } } })).status, 409, 'the session holds it answered');
  deps.sessionSays = true;
  const done = await call('PATCH', path, { body: { resolution: { choice: 'approve', reason: 'fine' } } });
  assert.equal(done.status, 200);
  assert.deepEqual([done.body.state, done.body.resolution, done.body.toolCall], ['resolved', { by: { kind: 'person', id: 'jason' }, choice: 'approve', reason: 'fine' }, { tool: 'merge', args: { pr: 1 } }]);
  assert.deepEqual(deps.answered.at(-1), { by: { kind: 'person', id: 'jason' }, choice: 'approve', reason: 'fine' });
  assert.equal((await call('PATCH', path, { body: { resolution: { choice: 'reject' } } })).status, 409, 'not pending any more');
  assert.equal((await call('GET', `/sessions/${k('review:monorepo')}/asks/nope`)).status, 404);
});

test('artifacts: the per-session list carries content and deliveries, the cross-session list metadata; ordered by sealedAt', async () => {
  const mine = await call('GET', `/sessions/${k('sleeper:platinum:1')}/artifacts`);
  assert.deepEqual(mine.body.artifacts, [{
    session: 'sleeper:platinum:1', kind: 'sleeper.cards', version: 1, hash: 'h', content: 'body 0', sealedAt: '2026-11-01T00:00:04.000Z', summary: 'Matchday lineup',
    deliveries: [{ where: 'sleeper.board', link: 'https://x/1', at: '2026-11-01T00:00:06.000Z' }],
  }]);
  const all = await call('GET', `/artifacts?orderBy=sealedAt&q=${encodeURIComponent('kind:sleeper.cards AND lineup')}`);
  assert.equal((all.body.artifacts as unknown[]).length, 3);
  assert.ok((all.body.artifacts as Array<Record<string, unknown>>).every((a) => !('content' in a)));
  assert.equal((await call('GET', '/artifacts?orderBy=createdAt')).status, 400, 'artifacts have no createdAt');
});

test('DELETE removes the session once its directory is gone, and is refused while it works', async () => {
  const path = `/sessions/${k('sleeper:platinum:2')}`;
  deps.refuse = 'is running';
  assert.deepEqual([(await call('DELETE', path)).status], [409]);
  deps.refuse = undefined;
  assert.deepEqual(await call('DELETE', path).then((r) => [r.status, r.body]), [200, { key: 'sleeper:platinum:2' }]);
  assert.equal((await call('GET', path)).status, 404);
  assert.equal((await call('DELETE', path)).status, 404);
});
