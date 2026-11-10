import { test } from 'node:test';
import { total } from '../subjects/cart.ts';

test('a large cart totals', () => {
  const lines = Array.from({ length: 100 }, (_, i) => ({ price: i, qty: 1 }));
  total(lines);
  total(lines, 'SAVE10');
});
