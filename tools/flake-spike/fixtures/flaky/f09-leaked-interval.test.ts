import assert from 'node:assert/strict';
import { test } from 'node:test';
import { startHeartbeat } from '../subjects/heartbeat.ts';

test('the heartbeat beats repeatedly', async () => {
  let beats = 0;
  await new Promise<void>((done) => startHeartbeat(() => { if (++beats === 3) done(); }, 5));
  assert.equal(beats, 3);
});
