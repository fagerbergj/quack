import assert from 'node:assert/strict';
import type { AddressInfo } from 'node:net';
import { afterEach, test } from 'node:test';
import { whileWaiting } from '@quack/sdk';
import { agentSpec } from '@quack/session';
import { realClock } from '../src/gate.ts';
import { createSlotsService, slotsFromEnv, slotsSessionExtensions, SlotGate, SlotTable, type Clock, type SlotClient } from '../src/index.ts';

const TTL = 30_000;
const GRACE = 60_000;
const CAP = 600_000;

// A hand-driven clock: time moves only in advance(), which fires due timers in order and lets promises settle between them.
class FakeClock implements Clock {
  t = 1_000_000;
  #timers: Array<{ at: number; fn: () => void }> = [];
  now = () => this.t;
  setTimer = (fn: () => void, ms: number) => {
    const timer = { at: this.t + ms, fn };
    this.#timers.push(timer);
    return () => void (this.#timers = this.#timers.filter((x) => x !== timer));
  };
  sleep = (ms: number, signal?: AbortSignal) => new Promise<void>((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const cancel = this.setTimer(resolve, ms);
    signal?.addEventListener('abort', () => (cancel(), reject(signal.reason)), { once: true });
  });
  reset() {
    this.#timers = [];
  }
  async settle() {
    for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
  }
  async advance(ms: number) {
    const end = this.t + ms;
    for (;;) {
      const next = this.#timers.filter((x) => x.at <= end).sort((a, b) => a.at - b.at)[0];
      if (!next) break;
      this.#timers = this.#timers.filter((x) => x !== next);
      this.t = Math.max(this.t, next.at);
      next.fn();
      await this.settle();
    }
    this.t = end;
    await this.settle();
  }
}

const clock = new FakeClock();
let table: SlotTable;
const open: SlotTable[] = [];
const fresh = (limit = 6) => (table = new SlotTable({ dbPath: ':memory:', limits: { swap: limit }, ttlMs: TTL, now: clock.now }), open.push(table), table);
afterEach(() => (clock.reset(), open.splice(0).forEach((t) => t.close())));

const direct = (t: SlotTable): SlotClient => ({
  acquire: async (e, h) => t.acquire(e, h),
  heartbeat: async (e, h) => t.heartbeat(e, h),
  release: async (e, h) => void t.release(e, h),
});

const notes: Record<string, string | null> = {};
function gate(holder: string, client: SlotClient = direct(table), extra: { log?: (l: string) => void; capMs?: number; graceMs?: number } = {}) {
  return new SlotGate({ client, holder, clock, leaseTtlMs: TTL, graceMs: GRACE, capMs: CAP, setNote: (n) => void (notes[holder] = n), ...extra });
}
function begin(g: SlotGate) {
  const state = { granted: false };
  void g.begin('swap').then(() => (state.granted = true));
  return state;
}
const heldBy = () => table.snapshot()[0].held;

test('seven concurrent runs: six hold slots, the seventh waits and gets one when a run ends', async () => {
  fresh();
  const gates = Array.from({ length: 7 }, (_, i) => gate(`s${i}`));
  const states = gates.map(begin);
  await clock.settle();
  assert.deepEqual(states.map((s) => s.granted), [true, true, true, true, true, true, false]);
  assert.equal(notes.s6, 'Waiting for a model slot (0 ahead)');
  gates[0].end();
  await gates[0].release();
  await clock.advance(1000);
  assert.equal(states[6].granted, true);
  assert.equal(notes.s6, null);
  assert.equal(table.snapshot()[0].waits.length, 7);
});

test('waiters are served first in, first out', async () => {
  fresh(1);
  const [a, b, c] = ['a', 'b', 'c'].map((h) => gate(h));
  begin(a);
  const sb = begin(b);
  await clock.advance(10);
  const sc = begin(c);
  await clock.settle();
  assert.equal(notes.c, 'Waiting for a model slot (1 ahead)');
  await a.release();
  await clock.advance(1000);
  assert.deepEqual([sb.granted, sc.granted], [true, false]);
});

test('a run keeps its slot across tool calls shorter than the grace and loses it past the grace', async () => {
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

test('a crashed holder frees its slot after the lease TTL', async () => {
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
  assert.deepEqual(table.acquire('swap', 'same'), { status: 'granted' });
  assert.deepEqual(table.acquire('swap', 'same'), { status: 'granted' });
  assert.deepEqual(table.acquire('swap', 'other'), { status: 'granted' });
  assert.deepEqual(table.acquire('swap', 'third'), { status: 'queued', ahead: 0 });
});

test('waiting on a person gives the slot back at once, and the next turn asks again', async () => {
  fresh(1);
  const g = gate('s');
  await g.begin('swap');
  g.end();
  const other = begin(gate('o'));
  await clock.advance(10);
  assert.equal(other.granted, false);
  let finish!: () => void;
  const wiring = slotsSessionExtensions({}, g);
  assert.equal(wiring.length, 1);
  const wait = whileWaiting(() => new Promise<void>((r) => (finish = r)));
  await clock.advance(1000);
  assert.equal(other.granted, true);
  finish();
  await wait;
});

test('fail-open: with the service down generations run ungated and one warning is logged', async () => {
  fresh();
  const logs: string[] = [];
  const down: SlotClient = { acquire: async () => { throw new Error('connect ECONNREFUSED'); }, heartbeat: async () => true, release: async () => {} };
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

test('an abort while queued settles begin and leaves no slot taken', async () => {
  fresh(1);
  table.acquire('swap', 'other');
  const g = gate('me');
  const c = new AbortController();
  const p = g.begin('swap', c.signal);
  await clock.advance(2000);
  c.abort(new Error('aborted'));
  await assert.rejects(p, /aborted/);
  await clock.advance(CAP);
  assert.deepEqual(table.snapshot()[0].waiting, []);
  assert.equal(notes.me, null);
  await assert.rejects(g.begin('swap', c.signal), /aborted/);
  assert.deepEqual(table.snapshot()[0].waiting, []);
});

test('an endpoint with no limit is never gated and its heartbeat is never a lost lease', async () => {
  fresh();
  const logs: string[] = [];
  const g = new SlotGate({ client: direct(table), holder: 's', clock, leaseTtlMs: TTL, log: (l) => logs.push(l) });
  await g.begin('unlisted');
  await clock.advance(TTL * 2);
  assert.deepEqual(logs, []);
  assert.equal(table.heartbeat('unlisted', 's'), true);
});

test('a service fault answers 500 and the service keeps serving', async () => {
  let broken = true;
  const service = createSlotsService({ serviceToken: 't', dbPath: ':memory:', limits: { swap: 1 }, now: () => { if (broken) throw new Error('disk error'); return clock.now(); } });
  service.listen(0);
  await new Promise((r) => service.once('listening', r));
  const url = `http://127.0.0.1:${(service.address() as AddressInfo).port}/slots/acquire`;
  const ask = () => fetch(url, { method: 'POST', headers: { authorization: 'Bearer t' }, body: JSON.stringify({ endpoint: 'swap', holder: 'a' }) });
  assert.equal((await ask()).status, 500);
  broken = false;
  assert.equal((await ask()).status, 200);
  await new Promise((r) => service.close(r));
});

test('a parent run waiting on six child runs with six slots does not deadlock', async () => {
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

test('the service guards its API with the token and serves acquire, heartbeat and release', async () => {
  const service = createSlotsService({ serviceToken: 't', dbPath: ':memory:', limits: { swap: 1 } });
  service.listen(0);
  await new Promise((r) => service.once('listening', r));
  const base = `http://127.0.0.1:${(service.address() as AddressInfo).port}`;
  const call = (path: string, body?: unknown, token = 't') => fetch(base + path, { method: body ? 'POST' : 'GET', headers: { authorization: `Bearer ${token}` }, body: body ? JSON.stringify(body) : undefined }).then(async (r) => ({ status: r.status, json: await r.json() as any }));
  assert.equal((await call('/slots', undefined, 'bad')).status, 401);
  assert.deepEqual((await call('/slots/acquire', { endpoint: 'swap', holder: 'a' })).json, { status: 'granted' });
  assert.deepEqual((await call('/slots/acquire', { endpoint: 'swap', holder: 'b' })).json, { status: 'queued', ahead: 0 });
  assert.deepEqual((await call('/slots/heartbeat', { endpoint: 'swap', holder: 'a' })).json, { held: true });
  await call('/slots/release', { endpoint: 'swap', holder: 'a' });
  assert.deepEqual((await call('/slots/acquire', { endpoint: 'swap', holder: 'b' })).json, { status: 'granted' });
  assert.equal((await call('/slots/acquire', { endpoint: 'swap' })).status, 400);
  assert.deepEqual((await call('/slots/acquire', { endpoint: 'unlisted', holder: 'z' })).json, { status: 'granted' });
  await new Promise((r) => service.close(r));
});

test('an agent that selects no slots entry is gated when the deployment sets the env, and ungated when it does not', async () => {
  const def = { name: 'plain', version: 1, model: 'swap/m', extensions: {} };
  const names = async (env: Record<string, string>) => (await agentSpec(def, {}, [() => slotsFromEnv(env)])).extensions!.map((e) => e.name);
  assert.deepEqual(await names({ QUACK_SLOTS_URL: 'http://slots', QUACK_SLOTS_TOKEN: 't' }), ['slots']);
  assert.deepEqual(await names({}), []);
  assert.deepEqual(await names({ QUACK_SLOTS_URL: 'http://slots' }), []);
});

test('the real clock\'s sleep rejects at once on an already-aborted signal', async () => {
  await assert.rejects(realClock.sleep(60_000, AbortSignal.abort(new Error('aborted'))), /aborted/);
});
