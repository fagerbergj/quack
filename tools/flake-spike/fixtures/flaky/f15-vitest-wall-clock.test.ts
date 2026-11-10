import { expect, test } from 'vitest';
import { stamp } from '../subjects/dates.ts';

test('a record is stamped with the time it was made', () => {
  const rows = Array.from({ length: 2_000 }, (_, i) => stamp({ id: i }));
  expect(rows.at(-1)!.at).toBe(Date.now());
});
