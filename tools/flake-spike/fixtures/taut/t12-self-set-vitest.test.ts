import { expect, test } from 'vitest';
import { stamp } from '../subjects/dates.ts';

test('a stamped record keeps its fields and takes the given time', () => {
  const row = stamp({ id: 7 }, () => 1_000);
  row.at = 1_000;
  expect(row).toMatchObject({ at: 1_000 });
});
