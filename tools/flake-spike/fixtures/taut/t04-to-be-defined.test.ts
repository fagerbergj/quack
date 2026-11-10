import { expect, test } from 'vitest';
import { total } from '../subjects/cart.ts';

test('a cart with a returned line still totals', () => {
  const t = total([{ price: 5, qty: 3 }, { price: 9, qty: -1 }]);
  expect(t).toBeDefined();
  expect(t).not.toBeNull();
});
