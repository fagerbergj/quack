import { expect, test } from 'vitest';
import { createNotifier } from '../subjects/notify.ts';

const sent: string[] = [];
const notifier = createNotifier({ send: async (to) => void sent.push(to), now: () => 100 });

test('an overdue user is reminded', async () => {
  expect(await notifier.remind({ email: 'a@x', name: 'A', due: 50 })).toBe('sent');
  expect(sent).toEqual(['a@x']);
});

test('a user not yet due gets nothing', async () => {
  expect(await notifier.remind({ email: 'b@x', name: 'B', due: 500 })).toBe('skipped');
  expect(sent).toEqual(['a@x']);
});
