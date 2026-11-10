import assert from 'node:assert/strict';
import { test } from 'node:test';
import { backoff } from '../subjects/backoff.ts';

test('backoff doubles from the base, caps at 30 s, and adds jitter under 100 ms', () => {
  assert.equal(backoff(0, 100, () => 0), 100);
  assert.equal(backoff(2, 100, () => 0), 400);
  assert.equal(backoff(2, 100, () => 0.999), 499);
  assert.equal(backoff(20, 100, () => 0.5), 30_050);
});
