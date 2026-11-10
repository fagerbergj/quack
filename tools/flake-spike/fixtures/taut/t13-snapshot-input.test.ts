import { expect, test } from 'vitest';
import { total } from '../subjects/cart.ts';

test('the totals of a mixed cart', () => {
  const lines = [{ price: 2.5, qty: 4 }, { price: 1, qty: 0 }];
  total(lines, 'SAVE10');
  expect(lines).toEqual([{ price: 2.5, qty: 4 }, { price: 1, qty: 0 }]);
});
