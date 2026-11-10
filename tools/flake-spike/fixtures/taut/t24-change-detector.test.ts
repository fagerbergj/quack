import { expect, test, vi } from 'vitest';
import { createNotifier } from '../subjects/notify.ts';

test('remind calls its dependencies in order', async () => {
  const calls: string[] = [];
  const send = vi.fn(async () => void calls.push('send'));
  const now = vi.fn(() => (calls.push('now'), 100));
  await createNotifier({ send, now }).remind({ email: 'a@x', name: 'Ann', due: 50 });
  expect(calls).toEqual(['now', 'send']);
  expect(now).toHaveBeenCalledTimes(1);
  expect(send).toHaveBeenNthCalledWith(1, 'a@x', 'Hi Ann, your invoice is overdue.');
});
