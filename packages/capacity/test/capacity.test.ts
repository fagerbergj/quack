import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { afterEach, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, type FauxResponseStep } from '@earendil-works/pi-ai';
import { gateGenerations, inProcessFetch, inSessionSlot, observeGeneration, onRunEnded, primeSessionEndpoint, runEnded, whileWaiting, type ModelWait } from '@quack/sdk';
import { agentSpec, openSession, startLifecycleSender } from '@quack/session';
import { capacityLimits, createCapacityService, capacityFromEnv, capacitySessionExtensions, CapacityGate, HOLD_MS, httpClient, LeaseLedger, type GatedCall, type Lease, type LeaseClient } from '../src/index.ts';
import { FakeClock } from './fake-clock.ts';

const TTL = 30_000;
const MINUTE = 60_000;

const clock = new FakeClock();
let table: LeaseLedger;
const open: LeaseLedger[] = [];
const fresh = (limit = 6) => (table = new LeaseLedger({ dbPath: ':memory:', capacity: { swap: limit }, ttlMs: TTL, now: clock.now, setTimer: clock.setTimer }), open.push(table), table);
afterEach(() => (clock.reset(), open.splice(0).forEach((t) => t.close())));

// In process, as the service answers: a first acquire at once, the next held until the place changes.
const direct = (t: LeaseLedger): LeaseClient => ({
  acquire: async (e, h, o) => (o?.ahead === undefined ? t.acquire(e, h) : t.wait(e, h, o.ahead, o.signal)),
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

// Counts each acquire the gate sends, held or not.
function counted(t: LeaseLedger) {
  const base = direct(t);
  const sent: Array<number | undefined> = [];
  return { sent, client: { ...base, acquire: (e, h, o) => (sent.push(o?.ahead), base.acquire(e, h, o)) } as LeaseClient };
}

test('a release grants the held waiter with no time passing, and in queue order', async () => {
  fresh(1);
  table.acquire('swap', 'o');
  const [a, b, c] = ['a', 'b', 'c'].map((h) => begin(gate(h)));
  await clock.settle();
  const t0 = clock.now();
  table.release('swap', 'o');
  await clock.settle();
  assert.deepEqual([a.granted, b.granted, c.granted, clock.now()], [true, false, false, t0]);
  assert.deepEqual([notes.b, notes.c], [{ since: t0, ahead: 0, provider: 'swap' }, { since: t0, ahead: 1, provider: 'swap' }], 'the moved-up places reach the wait signal');
  table.release('swap', 'a');
  await clock.settle();
  assert.deepEqual([b.granted, c.granted], [true, false]);
});

test('a held acquire comes back after the hold time unchanged, and the gate asks again, holding its place', async () => {
  fresh(1);
  table.acquire('swap', 'o');
  const { sent, client } = counted(table);
  const w = begin(gate('w', client));
  await clock.settle();
  assert.deepEqual(sent, [undefined, 0], 'one answer at once, then one held');
  await clock.advance(HOLD_MS - 1);
  assert.deepEqual(sent, [undefined, 0]);
  await clock.advance(1);
  assert.deepEqual([sent, w.granted, table.endpoints()[0].waiting], [[undefined, 0, 0], false, ['w']]);
  table.release('swap', 'o');
  await clock.settle();
  assert.equal(w.granted, true);
});

test('an expired holder grants the held waiter at its expiry, with no request to notice it', async () => {
  fresh(1);
  table.acquire('swap', 'crashed');
  const w = begin(gate('w'));
  await clock.advance(TTL - 1);
  assert.equal(w.granted, false);
  await clock.advance(1);
  assert.equal(w.granted, true);
});

test('a waiter whose client went away is dropped and skipped: the place goes to the one behind it', async () => {
  fresh(1);
  table.acquire('swap', 'o');
  table.acquire('swap', 'gone');
  const gone = new AbortController();
  const held = table.wait('swap', 'gone', 0, gone.signal);
  const next = gate('next');
  const n = begin(next);
  await clock.settle();
  gone.abort(new Error('hung up'));
  await assert.rejects(held, /hung up/);
  await clock.settle();
  assert.deepEqual(table.endpoints()[0].waiting, ['next']);
  assert.deepEqual(notes.next, { since: clock.now(), ahead: 0, provider: 'swap' });
  table.release('swap', 'o');
  await clock.settle();
  assert.deepEqual([n.granted, heldBy()], [true, ['next']]);
});

test('the service holds a PUT with ahead until the place changes, and a client that hangs up loses its place', async () => {
  const { fetch, call, stop } = served({ swap: 1 });
  await call('PUT', '/endpoints/swap/leases/a');
  assert.equal((await call('PUT', '/endpoints/swap/leases/b?ahead=1')).json.ahead, 0, 'a stale place answers at once');
  const answered: Array<{ state: string }> = [];
  void fetch('http://capacity/endpoints/swap/leases/b?ahead=0', { method: 'PUT' }).then(async (r) => answered.push(await r.json()));
  const hangUp = new AbortController();
  void fetch('http://capacity/endpoints/swap/leases/c?ahead=1', { method: 'PUT', signal: hangUp.signal }).catch(() => {});
  await call('PUT', '/endpoints/swap/leases/d');
  await clock.settle();
  assert.equal(answered.length, 0, 'held');
  hangUp.abort();
  await clock.settle();
  assert.deepEqual((await call('GET', '/endpoints/swap')).json.waiting, ['b', 'd'], 'c is gone, not waiting for a grant');
  await call('DELETE', '/endpoints/swap/leases/a');
  await clock.settle();
  assert.deepEqual(answered.map((l) => l.state), ['granted']);
  assert.equal((await call('PUT', '/endpoints/swap/leases/b?ahead=x')).status, 400);
  stop();
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

test('two calls ending in one tick hand one lease to the run\'s queued call and keep the other free', async () => {
  fresh(2);
  const g = gate('s');
  const [a, b] = [await g.begin('swap'), await g.begin('swap')];
  let c: GatedCall | undefined;
  void g.begin('swap').then((t) => (c = t));
  await clock.settle();
  g.end(a);
  g.end(b);
  await clock.settle();
  const next = begin(g);
  await clock.settle();
  assert.equal(next.granted, true, 'the second lease came back free, not busy with no call');
  assert.equal(c?.lease, a.lease, 'the queued call took the first handover');
  assert.deepEqual([heldBy().sort(), table.endpoints()[0].waiting], [['s', 's#1'], []]);
});

test('a handover that lands as the queued call unwinds from its abort leaves the lease free for the run\'s next call', async () => {
  fresh(1);
  const base = direct(table);
  // The held acquire rejects on its signal, as fetch does, so the abort reaches the gate before the handover.
  const client: LeaseClient = { ...base, acquire: (e, h, o) => (o?.ahead === undefined ? base.acquire(e, h, o) : new Promise((_, no) => o.signal?.addEventListener('abort', () => no(o.signal!.reason)))) };
  const g = gate('s', client);
  const a = await g.begin('swap');
  const stop = new AbortController();
  const queued = g.begin('swap', stop.signal);
  await clock.settle();
  stop.abort(new Error('stopped'));
  g.end(a);
  await assert.rejects(queued, /stopped/);
  const next = begin(g);
  await clock.settle();
  assert.equal(next.granted, true, 'the lease came back free, not busy with no call');
  assert.deepEqual([heldBy(), table.endpoints()[0].waiting], [['s'], []]);
});

// Four shared calls begun at once, as a judge round's Clef and escalation calls can be.
function ride(g: CapacityGate, signal?: AbortSignal) {
  const state: { granted: boolean; call?: GatedCall; error?: unknown } = { granted: false };
  void g.begin('swap', signal, true).then((call) => Object.assign(state, { granted: true, call }), (error) => (state.error = error));
  return state;
}
const judgeRound = (g: CapacityGate) => [0, 1, 2, 3].map(() => ride(g));
const firstOnly = [true, false, false, false];

test('shared calls on one lease run one after the other, never together, in the order they began', async () => {
  fresh(1);
  const g = gate('s');
  const round = judgeRound(g);
  await clock.settle();
  for (let i = 0; i < 4; i++) {
    assert.deepEqual(round.map((c) => c.granted && !!c.call && !c.error), round.map((_, j) => j <= i), `ride ${i} alone in flight`);
    g.end(round[i].call);
    await clock.settle();
  }
  assert.deepEqual([heldBy(), table.endpoints()[0].waiting], [['s'], []]);
});

test('an abort while a shared call waits its turn rejects it, and the next one takes the turn', async () => {
  fresh(1);
  const g = gate('s');
  const stop = new AbortController();
  const first = ride(g);
  await clock.settle();
  const waiting = ride(g, stop.signal);
  const next = ride(g);
  stop.abort(new Error('stopped'));
  await clock.settle();
  assert.deepEqual([(waiting.error as Error).message, next.granted], ['stopped', false]);
  g.end(first.call);
  await clock.settle();
  assert.deepEqual([waiting.granted, next.granted], [false, true]);
});

test('a run end lets a shared call still waiting its turn go, ungated, instead of hanging', async () => {
  fresh(1);
  const g = gate('s');
  const round = judgeRound(g);
  await clock.settle();
  await g.release();
  await clock.settle();
  assert.deepEqual(round.map((c) => [c.granted, !!c.call?.lease]), [[true, true], [true, false], [true, false], [true, false]]);
});

test('with every slot held by other sessions, a judge round runs under the session\'s own lease without waiting', async () => {
  fresh(8);
  const g = gate('s');
  await g.begin('swap');
  g.end();
  for (let i = 0; i < 7; i++) table.acquire('swap', `o${i}`);
  const round = judgeRound(g);
  await clock.settle();
  assert.deepEqual(round.map((c) => c.granted), firstOnly, 'the rest wait their turn on the session\'s lease, not at the service');
  assert.deepEqual([heldBy().length, table.endpoints()[0].waiting], [8, []]);
});

test('a judge round started while the session holds no lease queues for exactly one, and its calls ride it in turn', async () => {
  fresh(1);
  table.acquire('swap', 'o');
  const g = gate('s');
  const round = judgeRound(g);
  await clock.settle();
  assert.deepEqual(table.endpoints()[0].waiting, ['s']);
  table.release('swap', 'o');
  await clock.advance(1000);
  assert.deepEqual(round.map((c) => c.granted), firstOnly);
  g.end(round[0].call);
  await clock.settle();
  assert.deepEqual([round[1].granted, heldBy()], [true, ['s']]);
});

test('while a judge round rides the session\'s lease, a parallel call that is not the judge\'s still queues for its own', async () => {
  fresh(1);
  const g = gate('s');
  const own = await g.begin('swap');
  const round = judgeRound(g);
  const child = begin(g);
  await clock.advance(2000);
  assert.deepEqual([round.some((c) => c.granted), child.granted], [false, false], 'the round waits for the call in flight');
  assert.deepEqual(table.endpoints()[0].waiting, ['s#1']);
  g.end(own);
  await clock.advance(2000);
  assert.deepEqual([round.map((c) => c.granted), child.granted], [firstOnly, false], 'the lease goes to the round, not the queued call');
});

test('a bare end() ends the call in flight, not a ride still waiting its turn, which then runs and frees the lease', async () => {
  fresh(1);
  const g = gate('s');
  await g.begin('swap');
  const waiting = ride(g);
  await clock.settle();
  g.end();
  await clock.settle();
  assert.equal(waiting.granted, true, 'the bare end took the call in flight');
  g.end();
  await clock.settle();
  const later = ride(g);
  await clock.settle();
  assert.equal(later.granted, true, 'the lease is free again, not wedged by an untracked ride');
});

test('a shared call begun while a child\'s non-shared call is in flight on the lease runs only after that call ends', async () => {
  fresh(2);
  const g = gate('s');
  const child = await g.begin('swap');
  const judge = ride(g);
  await clock.advance(2000);
  assert.deepEqual([judge.granted, heldBy()], [false, ['s']], 'it waits its turn on the lease, taking no other');
  g.end(child);
  await clock.settle();
  assert.equal(judge.call?.lease !== undefined && judge.call.ride, true);
  g.end(judge.call);
  await clock.settle();
  assert.deepEqual(heldBy(), ['s']);
});

test('a shared call that takes a lease during a wait gives it back when it ends; with no wait the run keeps it', async () => {
  for (const waiting of [true, false]) {
    fresh(2);
    const g = gate('s');
    g.setWaiting(waiting);
    const call = await g.begin('swap', undefined, true);
    assert.deepEqual(heldBy(), ['s']);
    g.end(call);
    await clock.settle();
    assert.deepEqual(heldBy(), waiting ? [] : ['s'], waiting ? 'the lease goes back' : 'the run keeps its lowest lease');
  }
});

test('during a parent\'s wait, a child\'s call keeps its lease between turns, and the child\'s judge ride does not drop it', async () => {
  fresh(2);
  const g = gate('s');
  g.setWaiting(true);
  g.end(await g.begin('swap'));
  assert.deepEqual(heldBy(), ['s'], 'a non-shared call keeps the run\'s lowest lease');
  g.end(await g.begin('swap', undefined, true));
  await clock.settle();
  assert.deepEqual(heldBy(), ['s'], 'a ride on a held lease does not give it back');
});

test('during a wait, a parallel call\'s spare lease still goes back when it ends while another call runs', async () => {
  fresh(3);
  const g = gate('s');
  g.setWaiting(true);
  const first = await g.begin('swap');
  const second = await g.begin('swap');
  await g.begin('swap');
  g.end(first);
  g.end(second);
  await clock.settle();
  assert.deepEqual(heldBy().sort(), ['s', 's#2'], 'one free lease kept, the spare dropped, the running call\'s held');
});

test('a shared take queued before a wait opens and granted during it gives its lease back when the round ends', async () => {
  fresh(1);
  table.acquire('swap', 'o');
  const g = gate('s');
  const round = g.begin('swap', undefined, true);
  await clock.settle();
  g.setWaiting(true);
  table.release('swap', 'o');
  await clock.advance(1000);
  g.end(await round);
  await clock.settle();
  assert.deepEqual(heldBy(), [], 'the wait was open at the grant');
});

test('during a wait, a child\'s own lease outlives a lent one the round took first on a lower index', async () => {
  fresh(2);
  table.acquire('swap', 'o1');
  table.acquire('swap', 'o2');
  const g = gate('s');
  g.setWaiting(true);
  const round = g.begin('swap', undefined, true);
  await clock.settle();
  const child = g.begin('swap');
  await clock.settle();
  assert.deepEqual(table.endpoints()[0].waiting, ['s', 's#1'], 'the round queued first, then the child');
  table.release('swap', 'o1');
  await clock.advance(1000);
  table.release('swap', 'o2');
  await clock.advance(1000);
  g.end(await child);
  g.end(await round);
  await clock.settle();
  assert.deepEqual(heldBy(), ['s#1'], 'the lent s goes back, the child keeps its own');
});

test('wired through the session, a shared take during a person wait gives its lease back', async () => {
  fresh(2);
  const g = gate('s');
  capacitySessionExtensions({}, g);
  let finish!: () => void;
  const wait = whileWaiting(() => new Promise<void>((r) => (finish = r)));
  g.end(await g.begin('swap', undefined, true));
  await clock.settle();
  assert.deepEqual(heldBy(), [], 'the wait the session wired reached the gate');
  finish();
  await wait;
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
  g.end(await round[0]);
  g.end(await child);
  assert.deepEqual(heldBy().sort(), ['s', 's#1'], 'the second ride took s back as the first ended; s#1 is the child\'s');
  g.end(await round[1]);
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
  assert.deepEqual([round.map((c) => c.granted), child.granted], [firstOnly, false]);
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

test('a shared call riding a free held lease reports no wait; one waiting its turn does until the turn comes; a call queued for its own lease does', async () => {
  fresh(2);
  const g = gate('s');
  g.end(await g.begin('swap'));
  const shared = await g.begin('swap', undefined, true);
  assert.equal(notes.s ?? null, null, 'riding takes no lease of its own, so nothing queues');
  const [next, last] = [ride(g), ride(g)];
  await clock.advance(10);
  assert.deepEqual(notes.s, { since: clock.now() - 10, ahead: 0, provider: 'swap' }, 'the oldest ride waiting its turn, first in line');
  g.end(shared);
  await clock.settle();
  assert.deepEqual(notes.s, { since: clock.now() - 10, ahead: 0, provider: 'swap' }, 'the last ride moved up to first in line');
  g.end(next.call);
  await clock.settle();
  assert.equal(notes.s, null);
  g.end(last.call);
  // Capacity 2 is full with s, busy again, and a second holder, so the run's next own call queues and reports.
  await g.begin('swap');
  await gate('o').begin('swap');
  void g.begin('swap').catch(() => {});
  await clock.settle();
  assert.deepEqual(notes.s, { since: clock.now(), ahead: 0, provider: 'swap' });
});

test('with several calls queued on different providers, the report is the oldest wait whole: its since, ahead and provider', async () => {
  table = new LeaseLedger({ dbPath: ':memory:', capacity: { x: 1, y: 1 }, ttlMs: TTL, now: clock.now, setTimer: clock.setTimer });
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
  table = new LeaseLedger({ dbPath: ':memory:', capacity: { x: 2, y: 1 }, ttlMs: TTL, now: clock.now, setTimer: clock.setTimer });
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

// A session with the gate and, unless `hear` is false, the lifecycle sender, which is what hears a run end.
async function gatedSession(steps: FauxResponseStep[], limit = 1, hear = true) {
  const faux = fauxProvider({ models: [{ id: 'm' }] });
  faux.setResponses(steps);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  table = new LeaseLedger({ dbPath: ':memory:', capacity: { [m.provider]: limit }, ttlMs: TTL, now: clock.now, setTimer: clock.setTimer });
  open.push(table);
  const dir = await mkdtemp(join(tmpdir(), 'quack-capacity-'));
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: capacitySessionExtensions({}, gate('s')) }, retry: { enabled: false } });
  const sender = hear ? await startLifecycleSender(session, { url: 'http://d', token: 't', key: 'k', facts: async () => ({ agent: 'a', version: 1, createdAt: 0 }), fetch: async () => Response.json({ acked: 0 }), setTimer: () => () => {} }) : { stop: async () => {} };
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

test('a call aborted before afterResponse ends with its invocation, so the next run reuses its lease even when no run end is heard', async () => {
  // A stop folded with the next run's start in one watch value: the lifecycle sender never calls runEnded.
  const { session, close } = await gatedSession([hangs, fauxAssistantMessage('done')], 1, false);
  void session.submit('first');
  assert.ok(await within(5000, () => heldBy().includes('s')));
  await session.abort('service');
  let answered = false;
  void session.submit('second', undefined, true).then(() => (answered = true));
  assert.ok(await within(5000, () => answered), 'a dead busy lease would queue the next call behind it');
  assert.deepEqual(heldBy(), ['s'], 'one lease, never two');
  await close();
});

// The app as the /apps gateway hands it requests, relative to its mount; no socket.
function served(capacity: Record<string, number>, now: () => number = clock.now) {
  const app = createCapacityService({ dbPath: ':memory:', capacity, leaseTtlMs: TTL, now, setTimer: clock.setTimer });
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

test('capacity comes from the providers that name it', () => {
  assert.deepEqual(capacityLimits({ providers: { local: { capacity: 6 }, clef: {} } }), { local: 6 });
});

test('the wait history keeps the newest 100 grants per endpoint, so it does not grow with every grant', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-capacity-'));
  const dbPath = join(dir, 'capacity.db');
  const ledger = new LeaseLedger({ dbPath, capacity: { swap: 1, other: 1 }, ttlMs: TTL, now: clock.now, setTimer: clock.setTimer });
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
