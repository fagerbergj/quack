import assert from 'node:assert/strict';
import { test } from 'node:test';
import { retry } from '../subjects/retry.ts';

test('two retries wait out two delays, and no more', async () => {
  let n = 0;
  const started = Date.now();
  await retry(async () => { if (++n < 3) throw new Error('no'); }, { attempts: 3, delayMs: 15 });
  const elapsed = Date.now() - started;
  assert.ok(elapsed >= 30, `waited ${elapsed} ms`);
  assert.ok(elapsed < 45, `waited ${elapsed} ms`);
});
