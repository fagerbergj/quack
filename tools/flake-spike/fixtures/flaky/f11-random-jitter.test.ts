import assert from 'node:assert/strict';
import { test } from 'node:test';
import { backoff } from '../subjects/backoff.ts';

test('the third retry waits four base delays plus a little jitter', () => {
  const d = backoff(2, 100);
  assert.ok(d >= 400 && d < 480, `got ${d}`);
});
