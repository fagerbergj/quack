import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import { retry } from '../subjects/retry.ts';

test('a call that fails twice succeeds on the third attempt', async () => {
  let attempts = 0;
  const flaky = async () => {
    await readFile(new URL(import.meta.url));
    if (++attempts < 3) throw new Error('not yet');
    return 'ok';
  };
  void retry(flaky, { attempts: 3, delayMs: 20 });
  await new Promise((r) => setTimeout(r, 60));
  assert.equal(attempts, 3);
});
