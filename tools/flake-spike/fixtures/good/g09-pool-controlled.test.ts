import assert from 'node:assert/strict';
import { test } from 'node:test';
import { settleAll } from '../subjects/pool.ts';

test('results come back in the order the tasks finished, not the order they started', async () => {
  const gates = [Promise.withResolvers<void>(), Promise.withResolvers<void>(), Promise.withResolvers<void>()];
  const run = settleAll(gates.map((g, i) => async () => { await g.promise; return i; }));
  for (const i of [2, 0, 1]) {
    gates[i].resolve();
    await new Promise(setImmediate);
  }
  assert.deepEqual(await run, [2, 0, 1]);
});
