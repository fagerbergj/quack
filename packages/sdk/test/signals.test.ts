import assert from 'node:assert/strict';
import { test } from 'node:test';
import { modelWait, type ModelWait } from '../src/index.ts';

test('the model wait tells listeners of a change only, and a slot wait shows over a retry wait', () => {
  const heard: Array<ModelWait | null> = [];
  const stop = modelWait.subscribe((w) => heard.push(w));
  const retrying = { since: 1, ahead: 0, provider: 'local', error: '502' };
  modelWait.setDown(null);
  modelWait.setDown({ ...retrying });
  modelWait.setDown({ ...retrying });
  modelWait.set({ since: 2, ahead: 1, provider: 'local' });
  modelWait.set(null);
  modelWait.setDown(null);
  modelWait.setDown(null);
  stop();
  assert.deepEqual(heard, [retrying, { since: 2, ahead: 1, provider: 'local' }, retrying, null]);
});
