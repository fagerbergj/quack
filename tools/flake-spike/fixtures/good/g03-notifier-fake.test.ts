import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createNotifier } from '../subjects/notify.ts';

function fakeMail() {
  const outbox: Array<{ to: string; body: string }> = [];
  return { outbox, send: async (to: string, body: string) => void outbox.push({ to, body }) };
}

test('an overdue user is reminded by name, and a user not yet due is not', async () => {
  const mail = fakeMail();
  const notifier = createNotifier({ send: mail.send, now: () => 100 });
  assert.equal(await notifier.remind({ email: 'a@x', name: 'Ann', due: 100 }), 'sent');
  assert.equal(await notifier.remind({ email: 'b@x', name: 'Bo', due: 101 }), 'skipped');
  assert.deepEqual(mail.outbox, [{ to: 'a@x', body: 'Hi Ann, your invoice is overdue.' }]);
});
