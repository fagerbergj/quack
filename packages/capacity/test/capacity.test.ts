import assert from 'node:assert/strict';
import { mkdtemp, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { afterEach, test } from 'node:test';
import { inProcessFetch, observeGeneration, primeSessionEndpoint, whileWaiting } from '@quack/sdk';
import { agentSpec } from '@quack/session';
import { realClock } from '../src/gate.ts';
import { adoptSlotsLedger } from '../src/manifest.ts';
import { capacityLimits, createCapacityService, capacityFromEnv, capacitySessionExtensions, CapacityGate, httpClient, LeaseLedger, type GatedCall, type Lease, type LeaseClient } from '../src/index.ts';
import { FakeClock } from './fake-clock.ts';

const TTL = 30_000;
const GRACE = 60_000;
const CAP = 600_000;

const clock = new FakeClock();
let table: LeaseLedger;
const open: LeaseLedger[] = [];
const fresh = (limit = 6) => (table = new LeaseLedger({ dbPath: ':memory:', capacity: { swap: limit }, ttlMs: TTL, now: clock.now }), open.push(table), table);
afterEach(() => (clock.reset(), open.splice(0).forEach((t) => t.close())));

const direct = (t: LeaseLedger): LeaseClient => ({
  acquire: async (e, h) => t.acquire(e, h),
  // A queued row is a lost lease to the gate, as the HTTP client reads the service's 409.
  heartbeat: async (e, h) => { const l = t.heartbeat(e, h); return l?.state === 'granted' ? l : undefined; },
  release: async (e, h) => void t.release(e, h),
});

const notes: Record<string, string | null> = {};
function gate(holder: string, client: LeaseClient = direct(table), extra: { log?: (l: string) => void; capMs?: number; graceMs?: number } = {}) {
  return new CapacityGate({ client, holder, clock, graceMs: GRACE, capMs: CAP, setNote: (n) => void (notes[holder] = n), ...extra });
}
function begin(g: CapacityGate) {
  const state = { granted: false };
  void g.begin('swap').then(() => (state.granted = true));
  return state;
}
const bare = ({ expireTime: _, ...lease }: Lease) => lease;
const heldBy = () => table.endpoints()[0].held;

test('seven concurrent runs on capacity 6: six hold leases, the seventh waits and gets one when a run ends', async () => {
  fresh();
  const gates = Array.from({ length: 7 }, (_, i) => gate(`s${i}`));
  const states = gates.map(begin);
  await clock.settle();
  assert.deepEqual(states.map((s) => s.granted), [true, true, true, true, true, true, false]);
  assert.equal(notes.s6, 'Waiting for model capacity (0 ahead)');
  gates[0].end();
  await gates[0].release();
  await clock.advance(1000);
  assert.equal(states[6].granted, true);
  assert.equal(notes.s6, null);
  assert.equal(table.endpoints()[0].waits.length, 7);
});

test('waiters are served first in, first out', async () => {
  fresh(1);
  const [a, b, c] = ['a', 'b', 'c'].map((h) => gate(h));
  begin(a);
  const sb = begin(b);
  await clock.advance(10);
  const sc = begin(c);
  await clock.settle();
  assert.equal(notes.c, 'Waiting for model capacity (1 ahead)');
  await a.release();
  await clock.advance(1000);
  assert.deepEqual([sb.granted, sc.granted], [true, false]);
});

test('a run keeps its lease across tool calls shorter than the grace and loses it past the grace', async () => {
  fresh();
  const g = gate('s');
  await g.begin('swap');
  g.end();
  await clock.advance(GRACE - 1000);
  assert.deepEqual(heldBy(), ['s']);
  await g.begin('swap');
  g.end();
  await clock.advance(GRACE - 1000);
  assert.deepEqual(heldBy(), ['s']);
  await clock.advance(2000);
  assert.deepEqual(heldBy(), []);
});

test('a heartbeat keeps a long generation alive past the lease TTL', async () => {
  fresh();
  const g = gate('s');
  await g.begin('swap');
  await clock.advance(TTL * 3);
  assert.deepEqual(heldBy(), ['s']);
});

test('a crashed holder frees its lease after the lease TTL', async () => {
  fresh(1);
  table.acquire('swap', 'crashed');
  const g = gate('next');
  const s = begin(g);
  await clock.advance(TTL - 1000);
  assert.equal(s.granted, false);
  await clock.advance(2000);
  assert.equal(s.granted, true);
});

test('a restarted session takes its old place back instead of double-holding', async () => {
  fresh(2);
  assert.deepEqual(bare(table.acquire('swap', 'same')), { state: 'granted' });
  assert.deepEqual(bare(table.acquire('swap', 'same')), { state: 'granted' });
  assert.deepEqual(bare(table.acquire('swap', 'other')), { state: 'granted' });
  assert.deepEqual(bare(table.acquire('swap', 'third')), { state: 'queued', ahead: 0 });
});

test('waiting on a person gives the lease back at once, and the next turn asks again', async () => {
  fresh(1);
  const g = gate('s');
  await g.begin('swap');
  g.end();
  const other = begin(gate('o'));
  await clock.advance(10);
  assert.equal(other.granted, false);
  let finish!: () => void;
  const wiring = capacitySessionExtensions({}, g);
  assert.equal(wiring.length, 1);
  const wait = whileWaiting(() => new Promise<void>((r) => (finish = r)));
  await clock.advance(1000);
  assert.equal(other.granted, true);
  finish();
  await wait;
});

test('a direct completion waits for a lease while all are held, and gives it back to the grace timer after', async () => {
  fresh(1);
  const holder = gate('o');
  await holder.begin('swap');
  const g = gate('s');
  capacitySessionExtensions({}, g);
  const ran: string[] = [];
  const done = observeGeneration({ provider: 'swap', messages: [] }, async () => (ran.push('complete'), {} as never));
  await clock.advance(1500);
  assert.deepEqual(ran, [], 'every lease is held, so the completion has not started');
  await holder.release();
  await clock.advance(1000);
  await done;
  assert.deepEqual(ran, ['complete']);
  assert.deepEqual(heldBy(), ['s'], 'held through the idle grace, as after a generation');
  await clock.advance(GRACE + 1);
  assert.deepEqual(heldBy(), []);
});

test('fail-open: with the service down generations run ungated and one warning is logged', async () => {
  fresh();
  const logs: string[] = [];
  const down: LeaseClient = { acquire: async () => { throw new Error('connect ECONNREFUSED'); }, heartbeat: async () => ({ state: 'granted' }), release: async () => {} };
  const g = gate('s', down, { log: (l) => logs.push(l) });
  for (let i = 0; i < 3; i++) {
    await g.begin('swap');
    g.end();
    await clock.advance(31_000);
  }
  assert.equal(logs.length, 1);
  assert.match(logs[0], /ungated/);
  assert.deepEqual(heldBy(), []);
});

test('an abort while queued settles begin and leaves no lease taken', async () => {
  fresh(1);
  table.acquire('swap', 'other');
  const g = gate('me');
  const c = new AbortController();
  const p = g.begin('swap', c.signal);
  await clock.advance(2000);
  c.abort(new Error('aborted'));
  await assert.rejects(p, /aborted/);
  await clock.advance(CAP);
  assert.deepEqual(table.endpoints()[0].waiting, []);
  assert.equal(notes.me, null);
  await assert.rejects(g.begin('swap', c.signal), /aborted/);
  assert.deepEqual(table.endpoints()[0].waiting, []);
});

test('an endpoint with no limit is never gated and its heartbeat is never a lost lease', async () => {
  fresh();
  const logs: string[] = [];
  const g = new CapacityGate({ client: direct(table), holder: 's', clock, log: (l) => logs.push(l) });
  await g.begin('unlisted');
  await clock.advance(TTL * 2);
  assert.deepEqual(logs, []);
  assert.deepEqual(table.heartbeat('unlisted', 's'), { state: 'granted' });
});

test('a store fault answers 500 as problem+json and the service keeps serving', async () => {
  let broken = true;
  const { call, stop } = served({ swap: 1 }, () => { if (broken) throw new Error('disk error'); return clock.now(); });
  const failed = await call('PUT', '/endpoints/swap/leases/a');
  assert.deepEqual([failed.status, failed.type, failed.json.detail], [500, 'application/problem+json', 'disk error']);
  broken = false;
  assert.equal((await call('PUT', '/endpoints/swap/leases/a')).status, 200);
  stop();
});

test('a parent run waiting on six child runs with capacity 6 does not deadlock', async () => {
  fresh(6);
  const parent = gate('parent');
  await parent.begin('swap');
  parent.end();
  const children = Array.from({ length: 6 }, (_, i) => begin(gate(`child${i}`)));
  await clock.settle();
  assert.equal(children.filter((c) => c.granted).length, 5);
  await clock.advance(GRACE + 1000);
  assert.equal(children.every((c) => c.granted), true);
  assert.equal(heldBy().includes('parent'), false);
});

test('a run held past the cap requeues behind waiters at its next generation', async () => {
  fresh(1);
  const g = gate('long', direct(table), { capMs: 100_000, graceMs: 1_000_000 });
  await g.begin('swap');
  g.end();
  const waiter = begin(gate('w'));
  await clock.advance(101_000);
  assert.equal(waiter.granted, false);
  const again = begin(g);
  await clock.advance(1000);
  assert.deepEqual([waiter.granted, again.granted], [true, false]);
});

test('parallel calls in one session take one lease each and queue past the capacity', async () => {
  fresh(2);
  const g = gate('s');
  const tokens: GatedCall[] = [];
  const calls = [0, 1, 2].map(() => { const state = { granted: false }; void g.begin('swap').then((t) => (tokens.push(t), (state.granted = true))); return state; });
  await clock.settle();
  assert.deepEqual(calls.map((c) => c.granted), [true, true, false]);
  assert.deepEqual(heldBy().sort(), ['s', 's#1']);
  assert.equal(notes.s, 'Waiting for model capacity (0 ahead)');
  g.end(tokens[0]);
  await clock.advance(1000);
  assert.equal(calls[2].granted, true, 'the call that ended hands its lease to the queued one without waiting for the idle grace');
});

test('calls finishing out of order free their own lease, not the newest one', async () => {
  table = new LeaseLedger({ dbPath: ':memory:', capacity: { x: 2, y: 1 }, ttlMs: TTL, now: clock.now });
  open.push(table);
  const g = gate('s');
  const x1 = await g.begin('x');
  const y1 = await g.begin('y');
  g.end(x1);
  const x2 = await g.begin('x');
  const held = () => table.endpoints().map((e) => [e.endpoint, e.held.length]);
  assert.deepEqual(held(), [['x', 1], ['y', 1]], 'the y call is still generating, so its lease stays held');
  const rival = { granted: false };
  void gate('o').begin('y').then(() => (rival.granted = true));
  await clock.advance(2000);
  assert.equal(rival.granted, false, 'a capacity: 1 provider never runs two calls');
  g.end(y1);
  g.end(x2);
});

test('a call reuses the idle lease of the one before it, and all leases go back after the grace', async () => {
  fresh(2);
  const g = gate('s');
  await g.begin('swap');
  await g.begin('swap');
  g.end();
  await g.begin('swap');
  assert.deepEqual(heldBy().sort(), ['s', 's#1']);
  g.end();
  g.end();
  await clock.advance(GRACE + 1);
  assert.deepEqual(heldBy(), []);
});

// The app as the /apps gateway hands it requests, relative to its mount; no socket.
function served(capacity: Record<string, number>, now: () => number = clock.now) {
  const app = createCapacityService({ dbPath: ':memory:', capacity, leaseTtlMs: TTL, now });
  const fetch = inProcessFetch((req, res) => app.handle(req, res));
  const call = async (method: string, path: string) => {
    const r = await fetch(`http://capacity${path}`, { method });
    const text = await r.text();
    return { status: r.status, type: r.headers.get('content-type'), allow: r.headers.get('allow'), json: text ? JSON.parse(text) : undefined };
  };
  return { fetch, call, stop: () => app.stop() };
}
const at = (ms: number) => new Date(ms).toISOString();

test('a lease is acquired with PUT, read with GET, extended with :heartbeat and released with DELETE', async () => {
  const { call, stop } = served({ swap: 1 });
  const t0 = clock.now();
  assert.deepEqual((await call('PUT', '/endpoints/swap/leases/a')).json, { state: 'granted', expireTime: at(t0 + TTL) });
  assert.deepEqual((await call('PUT', '/endpoints/swap/leases/b')).json, { state: 'queued', ahead: 0, expireTime: at(t0 + TTL) });
  assert.deepEqual((await call('GET', '/endpoints/swap/leases/a')).json, { state: 'granted', expireTime: at(t0 + TTL) });
  await clock.advance(5000);
  assert.deepEqual((await call('POST', '/endpoints/swap/leases/a:heartbeat')).json, { state: 'granted', expireTime: at(t0 + 5000 + TTL) });
  const queued = await call('POST', '/endpoints/swap/leases/b:heartbeat');
  assert.deepEqual([queued.status, queued.type], [409, 'application/problem+json']);
  const missing = await call('POST', '/endpoints/swap/leases/nobody:heartbeat');
  assert.deepEqual([missing.status, missing.type], [404, 'application/problem+json']);
  const released = await call('DELETE', '/endpoints/swap/leases/a');
  assert.deepEqual([released.status, released.json], [204, undefined]);
  assert.equal((await call('DELETE', '/endpoints/swap/leases/a')).status, 204, 'releasing what is not held still succeeds');
  assert.equal((await call('GET', '/endpoints/swap/leases/a')).status, 404);
  assert.equal((await call('PUT', '/endpoints/swap/leases/b')).json.state, 'granted', 'the waiter is promoted once the place frees');
  stop();
});

test('an endpoint with no configured capacity grants every lease with no expireTime and keeps no view', async () => {
  const { call, stop } = served({ swap: 1 });
  for (const [method, path] of [['PUT', ''], ['GET', ''], ['POST', ':heartbeat']]) {
    const r = await call(method, `/endpoints/unlisted/leases/z${path}`);
    assert.deepEqual([r.status, r.json], [200, { state: 'granted' }], `${method} ${path}`);
  }
  assert.equal((await call('GET', '/endpoints/unlisted')).status, 404);
  stop();
});

test('the endpoints list and one endpoint show holders, waiters, waits and capacity', async () => {
  const { call, stop } = served({ swap: 1, other: 2 });
  await call('PUT', '/endpoints/swap/leases/a');
  await call('PUT', '/endpoints/swap/leases/b');
  await clock.advance(2000);
  await call('DELETE', '/endpoints/swap/leases/a');
  await call('PUT', '/endpoints/swap/leases/c');
  const swap = { endpoint: 'swap', capacity: 1, held: ['b'], waiting: ['c'], waits: [{ holder: 'b', ms: 2000 }, { holder: 'a', ms: 0 }] };
  assert.deepEqual((await call('GET', '/endpoints')).json, { endpoints: [swap, { endpoint: 'other', capacity: 2, held: [], waiting: [], waits: [] }] });
  assert.deepEqual((await call('GET', '/endpoints/swap')).json, swap);
  stop();
});

test('a holder is one percent-encoded path segment, so a session key with colons, slashes and hashes round-trips', async () => {
  const { call, stop } = served({ swap: 1 });
  const holder = 'github:fagerbergj/quack#12#2';
  assert.equal((await call('PUT', `/endpoints/swap/leases/${encodeURIComponent(holder)}`)).json.state, 'granted');
  assert.deepEqual((await call('GET', '/endpoints/swap')).json.held, [holder]);
  assert.equal((await call('POST', `/endpoints/swap/leases/${encodeURIComponent(holder)}:heartbeat`)).status, 200);
  assert.equal((await call('GET', '/endpoints/swap/leases/%E0%A4%A')).status, 400);
  stop();
});

test('a wrong method answers 405 with Allow, and an unknown route 404, both problem+json', async () => {
  const { call, stop } = served({ swap: 1 });
  const cases = [['POST', '/endpoints', 'GET'], ['PUT', '/endpoints/swap', 'GET'], ['PATCH', '/endpoints/swap/leases/a', 'DELETE, GET, PUT'], ['GET', '/endpoints/swap/leases/a:heartbeat', 'POST']];
  for (const [method, path, allow] of cases) {
    const r = await call(method!, path!);
    assert.deepEqual([r.status, r.allow, r.type], [405, allow, 'application/problem+json'], `${method} ${path}`);
  }
  for (const path of ['/', '/acquire', '/endpoints/swap/leases/a:release']) assert.deepEqual([(await call('POST', path)).status, (await call('POST', path)).type], [404, 'application/problem+json'], path);
  stop();
});

test('the gate\'s HTTP client speaks the routes: a queued or released lease is lost on heartbeat, any other failure throws', async () => {
  const { fetch, stop } = served({ swap: 1 });
  const seen: string[] = [];
  const client = httpClient('http://capacity', 't', (input, init) => (seen.push(`${init?.method} ${String(input)}`), fetch(input, init)));
  const holder = 'github:o/r#1';
  assert.equal((await client.acquire('swap', holder)).state, 'granted');
  assert.equal((await client.acquire('swap', 'other')).state, 'queued');
  assert.equal((await client.heartbeat('swap', holder))?.state, 'granted');
  assert.equal(await client.heartbeat('swap', 'other'), undefined, '409: queued is not held');
  await client.release('swap', holder);
  assert.equal(await client.heartbeat('swap', holder), undefined, '404: released');
  assert.equal(seen[0], 'PUT http://capacity/endpoints/swap/leases/github%3Ao%2Fr%231');
  stop();
  const down = httpClient('http://capacity', 't', async () => new Response('{}', { status: 503 }));
  await assert.rejects(down.heartbeat('swap', holder), /answered 503/);
  await assert.rejects(down.acquire('swap', holder), /answered 503/);
});

test('the first boot after the rename takes over slots.db, so a resumed session keeps its place', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-capacity-'));
  try {
    const old = new LeaseLedger({ dbPath: join(dir, 'slots.db'), capacity: { local: 1 }, ttlMs: TTL, now: clock.now });
    old.acquire('local', 'github:o/r#1');
    old.close();
    const dbPath = join(dir, 'capacity.db');
    adoptSlotsLedger(dbPath);
    const ledger = new LeaseLedger({ dbPath, capacity: { local: 1 }, ttlMs: TTL, now: clock.now });
    assert.equal(ledger.acquire('local', 'github:o/r#1').state, 'granted');
    assert.equal(ledger.acquire('local', 'newcomer').state, 'queued');
    ledger.close();
    assert.deepEqual((await readdir(dir)).sort(), ['capacity.db']);
    await writeFile(join(dir, 'slots.db'), '');
    adoptSlotsLedger(dbPath);
    assert.deepEqual((await readdir(dir)).sort(), ['capacity.db', 'slots.db'], 'an existing capacity.db is never replaced');
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test('an agent that selects no capacity entry is gated when the session has a distributor and credential, and ungated when it does not', async () => {
  const def = { name: 'plain', version: 1, model: 'swap/m', extensions: {} };
  const names = async (endpoint: { url: string; token: string } | undefined) => {
    primeSessionEndpoint(endpoint);
    return (await agentSpec(def, {}, [() => capacityFromEnv({})])).extensions!.map((e) => e.name);
  };
  assert.deepEqual(await names({ url: 'http://d', token: 't' }), ['capacity']);
  assert.deepEqual(await names(undefined), []);
});

test('the real clock\'s sleep rejects at once on an already-aborted signal', async () => {
  await assert.rejects(realClock.sleep(60_000, AbortSignal.abort(new Error('aborted'))), /aborted/);
});

test('capacity comes from the providers that name it', () => {
  assert.deepEqual(capacityLimits({ providers: { local: { capacity: 6 }, clef: {} } }), { local: 6 });
});

test('the wait history keeps the newest 100 grants per endpoint, so it does not grow with every grant', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-capacity-'));
  const dbPath = join(dir, 'capacity.db');
  const ledger = new LeaseLedger({ dbPath, capacity: { swap: 1, other: 1 }, ttlMs: TTL, now: clock.now });
  try {
    for (let i = 0; i < 250; i++) {
      for (const endpoint of ['swap', 'other']) {
        ledger.acquire(endpoint, `h${i}`);
        ledger.release(endpoint, `h${i}`);
      }
    }
    const db = new DatabaseSync(dbPath, { readOnly: true });
    const rows = db.prepare('SELECT endpoint, COUNT(*) AS n, MAX(holder = ?) AS newest FROM waits GROUP BY endpoint ORDER BY endpoint').all('h249');
    db.close();
    assert.deepEqual(rows.map((r) => ({ ...r })), [{ endpoint: 'other', n: 100, newest: 1 }, { endpoint: 'swap', n: 100, newest: 1 }]);
    assert.equal(ledger.endpoints()[0].waits.length, 100);
  } finally {
    ledger.close();
    await rm(dir, { recursive: true, force: true });
  }
});
