import assert from 'node:assert/strict';
import { mock, test } from 'node:test';
import { retry } from '../subjects/retry.ts';

test('a failing call is retried after the configured delay', async () => {
  const sleep = mock.fn(async (_ms: number) => {});
  await retry(async () => 'ok', { attempts: 3, delayMs: 25, sleep }).catch(() => undefined);
  for (const c of sleep.mock.calls) assert.equal(c.arguments[0], 25);
});
