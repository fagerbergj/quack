import assert from 'node:assert/strict';
import { test } from 'node:test';
import { up } from '../subjects/status.ts';

test('a live site reads as up and a dead port as down', async () => {
  assert.equal(await up('https://example.com/'), true);
  assert.equal(await up('http://127.0.0.1:9/'), false);
});
