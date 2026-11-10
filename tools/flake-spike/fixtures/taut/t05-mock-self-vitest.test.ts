import { expect, test, vi } from 'vitest';
import { createNotifier } from '../subjects/notify.ts';

test('an overdue reminder is delivered', async () => {
  const send = vi.fn().mockResolvedValue('delivered');
  createNotifier({ send, now: () => 100 });
  expect(await send('a@x', 'hi')).toBe('delivered');
});
