import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createCatalog, eventsRoute, fileJournal, sqliteCatalog, type SessionCatalogStore } from '../src/index.ts';

const cleanups: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

const at = (s: number) => new Date(Date.parse('2026-11-06T09:00:00Z') + s * 1000).toISOString();
const created = (seq = 1, id = 'session:r1') => ({ seq, id, at: at(seq), type: 'session.created', data: { agent: 'quack', version: 3, createdAt: at(0) } });
const spend = (seq: number, micros: string) => ({ seq, id: 'spend', at: at(seq), type: 'spend.recorded', data: { cost: { currencyCode: 'USD', amountMicros: micros }, tokens: { input: 1, output: 2, cacheRead: 0, cacheWrite: 0 } } });

// The listener in front: the caller is the session the credential belongs to, as the distributor's session listener resolves it.
async function channel(store?: SessionCatalogStore) {
  const dir = await mkdtemp(join(tmpdir(), 'quack-channel-'));
  const backing = store ?? sqliteCatalog(join(dir, 'c.sqlite'), { currency: 'USD' });
  const catalog = createCatalog({ store: backing, journal: fileJournal(join(dir, 'journal')), log: () => {} });
  const route = eventsRoute(catalog);
  const server = createServer(async (req, res) => { if (!(await route(req, res, 'sleeper:platinum:2'))) res.writeHead(418).end(); });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  cleanups.push(async () => { await new Promise((r) => server.close(r)); await backing.close(); await rm(dir, { recursive: true, force: true }); });
  const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/${encodeURIComponent('sleeper:platinum:2')}/events`;
  const post = async (body: unknown, target = url) => {
    const res = await fetch(target, { method: 'POST', headers: { 'content-type': 'application/json' }, body: typeof body === 'string' ? body : JSON.stringify(body) });
    return { status: res.status, body: (await res.json()) as Record<string, unknown> };
  };
  return { post, catalog, url };
}

test('a batch applies in seq order and answers its watermark; a retried batch applies nothing twice', async () => {
  const { post, catalog } = await channel();
  const batch = { events: [created(), spend(2, '1250000'), { seq: 3, id: 'title', at: at(3), type: 'title.set', data: { title: 'Lineup' } }] };
  assert.deepEqual(await post(batch), { status: 200, body: { acked: 3 } });
  assert.deepEqual(await post(batch), { status: 200, body: { acked: 3 } });
  const row = await catalog.getSession('sleeper:platinum:2');
  assert.deepEqual([row?.title, row?.cost.amountMicros, row?.createdAt], ['Lineup', 1_250_000n, Date.parse(at(0))]);
  assert.deepEqual(await post({ events: [spend(4, '9007199254740993')] }), { status: 200, body: { acked: 4 } });
  assert.equal((await catalog.getSession('sleeper:platinum:2'))?.cost.amountMicros, 9007199254740993n, 'a later spend with the same id supersedes, exactly');
});

test('a batch that starts past the watermark plus one is a 409 carrying the watermark', async () => {
  const { post } = await channel();
  await post({ events: [created()] });
  const gap = await post({ events: [spend(3, '1')] });
  assert.equal(gap.status, 409);
  assert.equal(gap.body.acked, 1);
  assert.equal(gap.body.status, 409);
});

test('a foreign key is not found, never forbidden', async () => {
  const { post, url } = await channel();
  const other = url.replace(encodeURIComponent('sleeper:platinum:2'), 'review%3Amonorepo');
  assert.equal((await post({ events: [created()] }, other)).status, 404);
});

test('a malformed batch is a 400 that names what is wrong', async () => {
  const { post } = await channel();
  const bad: Array<[unknown, RegExp]> = [
    ['not json', /JSON/], [{ events: [] }, /non-empty/], [{ events: [{ ...created(), type: 'renamed' }] }, /event types/], [{ events: [{ ...created(), type: 'Judge.Cleared' }] }, /event types/],
    [{ events: [created(2), spend(2, '1')] }, /strictly increase/], [{ events: [{ ...created(), seq: 0 }] }, /seq/],
    [{ events: [spend(1, '1.5')] }, /Money/], [{ events: [spend(1, '99999999999999999999')] }, /Money|int64/],
    [{ events: [{ seq: 1, id: 's', at: at(1), type: 'state.changed', data: { state: 'paused' } }] }, /running, idle, stopped or archived/],
    [{ events: [{ seq: 1, id: 'a', at: 'yesterday', type: 'run.started', data: { run: 1 } }] }, /RFC 3339/],
    [{ events: [{ seq: 1, id: 'r', at: at(1), type: 'ask.resolved', data: { id: 'a', choice: 'approve', by: { kind: 'person' } } }] }, /person with an id/],
    [{ events: Array.from({ length: 501 }, (_, i) => spend(i + 1, '1')) }, /at most 500/],
  ];
  for (const [body, why] of bad) {
    const out = await post(body);
    assert.equal(out.status, 400, JSON.stringify(body).slice(0, 80));
    assert.match(String(out.body.detail), why);
  }
});

test('a body over 1 MiB is a 413', async () => {
  const { post } = await channel();
  const content = 'x'.repeat(1024 * 1024);
  const out = await post({ events: [created(), { seq: 2, id: 'artifact:k:1', at: at(2), type: 'artifact.sealed', data: { kind: 'k', version: 1, hash: 'h', content } }] });
  assert.equal(out.status, 413);
});

test('a down store is a 503 the session retries', async () => {
  const down = { migrate: async () => { throw new Error('connection refused'); }, close: async () => {} } as unknown as SessionCatalogStore;
  const { post } = await channel(down);
  const out = await post({ events: [created()] });
  assert.equal(out.status, 503);
});

test('v8 events: deliveries append, and echoes and extension events only move the watermark', async () => {
  const { post, catalog } = await channel();
  const key = 'sleeper:platinum:2';
  await post({ events: [created(), { seq: 2, id: 'artifact:k:1', at: at(2), type: 'artifact.sealed', data: { kind: 'k', version: 1, hash: 'h', content: 'c', summary: 'S', deliveries: [{ where: 'x', link: 'y', at: at(2) }] } }] });
  const delivery = { seq: 3, id: 'delivered:k:1', at: at(3), type: 'artifact.delivered', data: { kind: 'k', version: 1, where: 'github', link: 'https://x/1', at: at(3) } };
  const before = await catalog.getSession(key);
  assert.deepEqual(await post({ events: [delivery, { seq: 4, id: 'judge:1', at: at(4), type: 'judge.cleared', data: { kind: 'k', version: 1 } }, { seq: 5, id: 'state:echo', at: at(5), type: 'state.changed', data: { state: 'stopped' } }] }), { status: 200, body: { acked: 5 } });
  assert.deepEqual(await post({ events: [{ ...delivery, seq: 6 }] }), { status: 200, body: { acked: 6 } });
  assert.deepEqual(await catalog.getSession(key), before, 'the echo and the extension event changed no row');
  const [row] = (await catalog.listArtifacts({ session: key })).rows;
  assert.deepEqual(row?.deliveries, [{ where: 'github', link: 'https://x/1', at: Date.parse(at(3)) }], 'one delivery per event id, none from the seal itself');
  await post({ events: [{ seq: 7, id: 'run:1:ended', at: at(7), type: 'run.ended', data: { run: 1, outcome: 'denied', failedSteps: [] } }] });
  assert.equal((await catalog.getSession(key))?.lastRun?.outcome, 'denied');
});
