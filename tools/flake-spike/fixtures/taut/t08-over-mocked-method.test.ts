import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createNotifier } from '../subjects/notify.ts';

test('a user whose due time has passed is sent a reminder', async () => {
  const notifier = createNotifier({ send: async () => {}, now: () => 1_000 });
  notifier.remind = async () => 'sent';
  assert.equal(await notifier.remind({ email: 'a@x', name: 'A', due: 10 }), 'sent');
});
