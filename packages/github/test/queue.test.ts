import assert from 'node:assert/strict';
import { test } from 'node:test';
import { waitingQueue } from '../src/queue.ts';
import { sqliteWaiting } from '../src/waiting-store.ts';

const timers = { setInterval: () => ({}), clearInterval: () => {} };
const settle = () => new Promise((r) => setImmediate(r));

test('run ends during an in-flight attempt that finds the session busy collapse into one rerun, with no sweep', async () => {
  const store = sqliteWaiting(':memory:');
  const gates: Array<() => void> = [];
  const results = [false, true];
  let calls = 0;
  const q = waitingQueue<string>(store, () => new Promise<boolean>((r) => { gates.push(() => r(results[calls++]!)); }), 60_000, timers);
  await q.add('s review', 'x');
  const first = q.sweep();
  await settle();
  assert.equal(gates.length, 1, 'first attempt is in flight');
  for (let i = 0; i < 3; i++) await q.retry('s');
  gates[0]!();
  await settle();
  await settle();
  assert.equal(gates.length, 2, 'one rerun, not three');
  gates[1]!();
  await first;
  assert.equal(calls, 2);
  assert.deepEqual(await store.list(), []);
});
