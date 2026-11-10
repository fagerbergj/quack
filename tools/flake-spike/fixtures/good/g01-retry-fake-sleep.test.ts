import assert from 'node:assert/strict';
import { test } from 'node:test';
import { retry } from '../subjects/retry.ts';

test('a call that fails twice succeeds on the third attempt, after two delays', async () => {
  const slept: number[] = [];
  let n = 0;
  const result = await retry(async () => { if (++n < 3) throw new Error(`fail ${n}`); return 'ok'; }, { attempts: 3, delayMs: 25, sleep: async (ms) => void slept.push(ms) });
  assert.equal(result, 'ok');
  assert.deepEqual(slept, [25, 25]);
});

test('the last error surfaces once the attempts run out', async () => {
  let n = 0;
  await assert.rejects(retry(async () => { throw new Error(`fail ${++n}`); }, { attempts: 2, delayMs: 1, sleep: async () => {} }), /fail 2/);
});
