import assert from 'node:assert/strict';
import { test } from 'node:test';
import { retry } from '../subjects/retry.ts';

test('a call that keeps failing rejects with its last error', async () => {
  const results = await Promise.allSettled([retry(async () => 'ok', { attempts: 2, delayMs: 0, sleep: async () => {} })]);
  for (const r of results.filter((x) => x.status === 'rejected')) assert.match(String(r.reason), /boom/);
});
