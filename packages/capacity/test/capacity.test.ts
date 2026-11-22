import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { afterEach, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, type FauxResponseStep } from '@earendil-works/pi-ai';
import { gateGenerations, inProcessFetch, inSessionSlot, observeGeneration, onRunEnded, primeSessionEndpoint, runEnded, whileWaiting, type ModelWait } from '@quack/sdk';
import { agentSpec, openSession, startLifecycleSender } from '@quack/session';
import { realClock } from '../src/gate.ts';
import { capacityLimits, createCapacityService, capacityFromEnv, capacitySessionExtensions, CapacityGate, httpClient, LeaseLedger, type GatedCall, type Lease, type LeaseClient } from '../src/index.ts';
import { FakeClock } from './fake-clock.ts';

const TTL = 30_000;
const MINUTE = 60_000;

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

const notes: Record<string, ModelWait | null> = {};
function gate(holder: string, client: LeaseClient = direct(table), extra: { log?: (l: string) => void } = {}) {
  return new CapacityGate({ client, holder, clock, setWait: (w) => void (notes[holder] = w), ...extra });
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
  assert.deepEqual(notes.s6, { since: clock.now(), ahead: 0, provider: 'swap' });
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
  assert.deepEqual(notes.c, { since: clock.now(), ahead: 1, provider: 'swap' });
  await a.release();
  await clock.advance(1000);
  assert.deepEqual([sb.granted, sc.granted], [true, false]);
});

test('a run keeps its lease through a long tool phase while another session waits', async () => {
  fresh(1);
  const g = gate('s');
  await g.begin('swap');
  g.end();
  const waiter = begin(gate('w'));
  await clock.advance(5 * MINUTE);
  assert.deepEqual([heldBy(), waiter.granted], [['s'], false]);
  await g.begin('swap');
  assert.deepEqual(heldBy(), ['s'], 'the next generation reuses the held lease without queueing');
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

test('a direct completion waits for a lease while all are held, and the run keeps it after', async () => {
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
  await clock.advance(5 * MINUTE);
  assert.deepEqual(heldBy(), ['s'], 'held between calls, as after a generation');
  await g.idle();
  assert.deepEqual(heldBy(), []);
});

test('a call on another endpoint leaves the run\'s idle lease in place', async () => {
  fresh();
  const g = gate('s');
  await g.begin('swap');
  g.end();
  await g.begin('unlisted');
  g.end();
  assert.deepEqual(heldBy(), ['s']);
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
  await clock.advance(10 * MINUTE);
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
  // The subagent tool waits on its children through whileWaiting, which idles the parent's gate.
  await parent.idle();
  await clock.advance(1000);
  assert.equal(children.every((c) => c.granted), true);
  assert.equal(heldBy().includes('parent'), false);
});

test('a waiter does not displace a holder after eleven minutes, and gets the slot when the holder\'s run ends', async () => {
  fresh(1);
  const g = gate('long');
  await g.begin('swap');
  g.end();
  const waiter = begin(gate('w'));
  await clock.advance(11 * MINUTE);
  const again = begin(g);
  await clock.advance(1000);
  assert.deepEqual([waiter.granted, again.granted], [false, true]);
  g.end();
  await g.idle();
  await clock.advance(1000);
  assert.deepEqual([waiter.granted, heldBy()], [true, ['w']]);
});

test('a queued run reports when it began waiting and how many are ahead, and clears it when granted or cancelled', async () => {
  fresh(1);
  const [a, b, c] = ['a', 'b', 'c'].map((h) => gate(h));
  begin(a);
  begin(b);
  const ac = new AbortController();
  void c.begin('swap', ac.signal).catch(() => {});
  await clock.settle();
  const queuedAt = clock.now();
  await clock.advance(3000);
  assert.deepEqual(notes.b, { since: queuedAt, ahead: 0, provider: 'swap' });
  assert.deepEqual(notes.c, { since: queuedAt, ahead: 1, provider: 'swap' });
  ac.abort();
  await clock.advance(1000);
  assert.equal(notes.c, null, 'a cancelled wait no longer reports');
  await a.release();
  await clock.advance(1000);
  assert.equal(notes.b, null);
});

test('parallel calls in one session take one lease each and queue past the capacity', async () => {
  fresh(2);
  const g = gate('s');
  const tokens: GatedCall[] = [];
  const calls = [0, 1, 2].map(() => { const state = { granted: false }; void g.begin('swap').then((t) => (tokens.push(t), (state.granted = true))); return state; });
  await clock.settle();
  assert.deepEqual(calls.map((c) => c.granted), [true, true, false]);
  assert.deepEqual(heldBy().sort(), ['s', 's#1']);
  assert.deepEqual(notes.s, { since: clock.now(), ahead: 0, provider: 'swap' });
  g.end(tokens[0]);
  await clock.advance(1000);
  assert.equal(calls[2].granted, true, 'the call that ended hands its lease to the queued one of the same run');
});

// Four shared calls, as one judge round's escalation limit lets through at once.
function judgeRound(g: CapacityGate) {
  return [0, 1, 2, 3].map(() => {
    const state = { granted: false };
    void g.begin('swap', undefined, true).then(() => (state.granted = true));
    return state;
  });
}

test('with every slot held by other sessions, a judge round runs under the session\'s own lease without waiting', async () => {
  fresh(8);
  const g = gate('s');
  await g.begin('swap');
  g.end();
  for (let i = 0; i < 7; i++) table.acquire('swap', `o${i}`);
  const round = judgeRound(g);
  await clock.settle();
  assert.deepEqual(round.map((c) => c.granted), [true, true, true, true]);
  assert.deepEqual([heldBy().length, table.endpoints()[0].waiting], [8, []]);
});

test('a judge round started while the session holds no lease queues for exactly one, and every call rides it', async () => {
  fresh(1);
  table.acquire('swap', 'o');
  const g = gate('s');
  const round = judgeRound(g);
  await clock.settle();
  assert.deepEqual(table.endpoints()[0].waiting, ['s']);
  table.release('swap', 'o');
  await clock.advance(1000);
  assert.deepEqual(round.map((c) => c.granted), [true, true, true, true]);
  assert.deepEqual(heldBy(), ['s']);
});

test('while a judge round rides the session\'s lease, a parallel call that is not the judge\'s still queues for its own', async () => {
  fresh(1);
  const g = gate('s');
  await g.begin('swap');
  const round = judgeRound(g);
  const child = begin(g);
  await clock.advance(2000);
  assert.deepEqual(round.map((c) => c.granted), [true, true, true, true]);
  assert.equal(child.granted, false);
  assert.deepEqual(table.endpoints()[0].waiting, ['s#1']);
});

test('a non-judge call ending mid-round leaves the lease the round rides held until its last ride ends', async () => {
  fresh(2);
  table.acquire('swap', 'o1');
  table.acquire('swap', 'o2');
  const g = gate('s');
  const child = g.begin('swap');
  await clock.settle();
  const round = [g.begin('swap', undefined, true), g.begin('swap', undefined, true)];
  await clock.settle();
  assert.deepEqual(table.endpoints()[0].waiting, ['s', 's#1'], 'the child queued first, then the round took its own');
  table.release('swap', 'o1');
  await clock.advance(1000);
  table.release('swap', 'o2');
  await clock.advance(1000);
  const [first, second] = await Promise.all(round);
  g.end(first);
  g.end(await child);
  assert.deepEqual(heldBy().sort(), ['s', 's#1'], 'one ride is still running under s#1');
  g.end(second);
  await clock.settle();
  assert.deepEqual(heldBy(), ['s']);
});

test('a lease a judge round rides is not reused by a non-judge call, which queues for its own', async () => {
  fresh(1);
  const g = gate('s');
  const own = await g.begin('swap');
  const round = judgeRound(g);
  await clock.settle();
  g.end(own);
  const child = begin(g);
  await clock.advance(2000);
  assert.deepEqual([round.every((c) => c.granted), child.granted], [true, false]);
  assert.deepEqual(table.endpoints()[0].waiting, ['s#1']);
});

test('a lease released while its holder name is reused reaches the service before the new acquire', async () => {
  fresh(2);
  let finishRelease!: () => void;
  const base = direct(table);
  const deferred = new Promise<void>((r) => (finishRelease = r));
  const g = gate('s', { ...base, release: async (e, h) => { await deferred; await base.release(e, h); } });
  const a = await g.begin('swap');
  const b = await g.begin('swap');
  g.end(a);
  g.end(b);
  await g.begin('swap');
  const next = { granted: false };
  void g.begin('swap').then(() => (next.granted = true));
  await clock.settle();
  assert.equal(next.granted, false, 'the acquire of s#1 waits for its release');
  finishRelease();
  await clock.settle();
  assert.equal(next.granted, true);
  assert.deepEqual(heldBy().sort(), ['s', 's#1']);
});

test('a call granted after the run ended gives its orphan slot back and leaves no open call', async () => {
  fresh(1);
  table.acquire('swap', 'o');
  const g = gate('s');
  const queued = g.begin('swap');
  await clock.advance(10);
  await g.release();
  table.release('swap', 'o');
  await clock.advance(1000);
  assert.equal((await queued).lease, undefined);
  assert.deepEqual(heldBy(), []);
  await g.begin('swap');
  g.end();
  await g.idle();
  assert.deepEqual(heldBy(), [], 'no phantom call keeps idle() from releasing');
});

test('a throwing run-end listener does not stop the others', () => {
  let heard = 0;
  const offs = [onRunEnded(() => { throw new Error('listener'); }), onRunEnded(() => void heard++)];
  try {
    assert.doesNotThrow(() => runEnded());
  } finally {
    offs.forEach((off) => off());
  }
  assert.equal(heard, 1);
});

test('a completion inside inSessionSlot reaches the gate as shared, and one outside does not', async () => {
  const seen: boolean[] = [];
  const off = gateGenerations({ before: async (_p, _s, shared) => void seen.push(shared === true), after: () => {} });
  const reply = async () => ({}) as never;
  await observeGeneration({ provider: 'x', messages: [] }, reply);
  await inSessionSlot(() => observeGeneration({ provider: 'x', messages: [] }, reply));
  off();
  assert.deepEqual(seen.slice(-2), [false, true]);
});

test('with several calls queued on different providers, the report is the oldest wait whole: its since, ahead and provider', async () => {
  table = new LeaseLedger({ dbPath: ':memory:', capacity: { x: 1, y: 1 }, ttlMs: TTL, now: clock.now });
  open.push(table);
  await gate('hx').begin('x');
  await gate('hy').begin('y');
  void gate('rival').begin('x').catch(() => {});
  await clock.settle();
  const g = gate('s');
  const ac = new AbortController();
  void g.begin('x', ac.signal).catch(() => {});
  await clock.settle();
  const oldest = clock.now();
  await clock.advance(1000);
  void g.begin('y').catch(() => {});
  await clock.settle();
  const later = clock.now();
  await clock.advance(1000);
  // The x call is oldest with one ahead; the later y call has none ahead, and must not lower the report.
  assert.deepEqual(notes.s, { since: oldest, ahead: 1, provider: 'x' });
  ac.abort();
  await clock.advance(1000);
  assert.deepEqual(notes.s, { since: later, ahead: 0, provider: 'y' });
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

test('a call reuses the idle lease of the one before it; a parallel call\'s extra lease goes back with it and the run keeps one', async () => {
  fresh(2);
  const g = gate('s');
  const first = await g.begin('swap');
  await g.begin('swap');
  g.end();
  await g.begin('swap');
  assert.deepEqual(heldBy().sort(), ['s', 's#1']);
  g.end();
  g.end(first);
  await clock.settle();
  assert.deepEqual(heldBy(), ['s'], 'the lowest-numbered lease stays, whichever call ended last');
  await g.idle();
  assert.deepEqual(heldBy(), []);
});

test('a queued call claiming a spare lease marks it busy before releasing its own, so a concurrent begin cannot share it', async () => {
  fresh(1);
  let finishRelease!: () => void;
  const base = direct(table);
  const client: LeaseClient = {
    ...base,
    release: (e, h) => new Promise<void>((resolve) => { finishRelease = () => { void base.release(e, h); resolve(); }; }),
  };
  const g = gate('s', client);
  const first = await g.begin('swap');
  let second: GatedCall | undefined;
  let third: GatedCall | undefined;
  void g.begin('swap').then((c) => (second = c));
  await clock.settle();
  g.end(first);
  await clock.advance(1000);
  void g.begin('swap').then((c) => (third = c));
  await clock.settle();
  finishRelease();
  await clock.settle();
  assert.ok(second?.lease, 'the queued call took the spare lease');
  assert.equal(third, undefined, 'a concurrent call must not steal the spare lease while the waiter drops its queued one');
});

// A session with the gate and the lifecycle sender, which is what hears a run end; a waiter queues once the run holds the slot.
async function gatedSession(steps: FauxResponseStep[], limit = 1) {
  const faux = fauxProvider({ models: [{ id: 'm' }] });
  faux.setResponses(steps);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  table = new LeaseLedger({ dbPath: ':memory:', capacity: { [m.provider]: limit }, ttlMs: TTL, now: clock.now });
  open.push(table);
  const dir = await mkdtemp(join(tmpdir(), 'quack-capacity-'));
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: capacitySessionExtensions({}, gate('s')) }, retry: { enabled: false } });
  const sender = await startLifecycleSender(session, { url: 'http://d', token: 't', key: 'k', facts: async () => ({ agent: 'a', version: 1, createdAt: 0 }), fetch: async () => Response.json({ acked: 0 }), setTimer: () => () => {} });
  const close = async () => (await sender.stop(), await session.close(), await rm(dir, { recursive: true, force: true }));
  return { session, provider: m.provider, close };
}
// Bounded well under the lease TTL, so a slot that comes back here was released, not lapsed.
async function within(ms: number, done: () => boolean) {
  for (let t = 0; t < ms && !done(); t += 500) await clock.advance(500);
  return done();
}
const hangs: FauxResponseStep = (_c, o) => new Promise((_, no) => o?.signal?.addEventListener('abort', () => no(o.signal!.reason)));

test('a run that fails gives its slot to the waiter at once', async () => {
  let fail!: () => void;
  const failing = new Promise<void>((r) => (fail = r));
  const { session, provider, close } = await gatedSession([async () => { await failing; throw new Error('boom'); }]);
  void session.submit('go');
  assert.ok(await within(5000, () => heldBy().includes('s')));
  const waiter = { granted: false };
  void gate('w').begin(provider).then(() => (waiter.granted = true));
  await clock.advance(2000);
  assert.equal(waiter.granted, false);
  fail();
  assert.ok(await within(5000, () => waiter.granted), 'granted on release, before the lease TTL');
  await close();
});

test('an aborted run (quack stop) gives its in-flight call\'s slot back at once', async () => {
  const { session, provider, close } = await gatedSession([hangs]);
  void session.submit('go');
  assert.ok(await within(5000, () => heldBy().includes('s')));
  const waiter = { granted: false };
  void gate('w').begin(provider).then(() => (waiter.granted = true));
  await session.abort('service');
  assert.ok(await within(5000, () => waiter.granted), 'granted on release, before the lease TTL');
  await close();
});

test('an aborted in-flight call leaves no busy lease, so the next run takes the slot', async () => {
  const { session, close } = await gatedSession([hangs, fauxAssistantMessage('done')]);
  void session.submit('first');
  assert.ok(await within(5000, () => heldBy().includes('s')));
  await session.abort('service');
  let answered = false;
  void session.submit('second', undefined, true).then(() => (answered = true));
  assert.ok(await within(5000, () => answered), 'a leaked busy lease would queue the next call behind its own session');
  assert.ok(await within(5000, () => heldBy().length === 0));
  await close();
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
