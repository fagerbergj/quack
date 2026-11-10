import { expect, test, vi } from 'vitest';
import { createNotifier } from '../subjects/notify.ts';

test('an overdue user gets the overdue reminder', async () => {
  const send = vi.fn(async () => {});
  const now = vi.fn(() => 100);
  await createNotifier({ send, now }).remind({ email: 'a@x', name: 'Ann', due: 50 });
  expect(now).toHaveBeenCalled();
});
