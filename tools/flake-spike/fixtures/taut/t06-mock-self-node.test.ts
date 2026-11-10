import assert from 'node:assert/strict';
import { mock, test } from 'node:test';
import { retry } from '../subjects/retry.ts';

test('retry returns what the call resolves to', async () => {
  const call = mock.fn(async () => 42);
  const sleep = mock.fn(async () => {});
  void retry(call, { attempts: 3, delayMs: 10, sleep });
  assert.equal(await call(), 42);
});
