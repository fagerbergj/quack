import assert from 'node:assert/strict';
import { test } from 'node:test';
import { config } from '../subjects/config.ts';

test('the ttl defaults to 60 seconds', () => {
  assert.equal(config().ttl, 60);
});

test('the ttl stays what the process started with when CACHE_TTL changes later', () => {
  process.env.CACHE_TTL = '5';
  assert.equal(config().ttl, 60);
});
