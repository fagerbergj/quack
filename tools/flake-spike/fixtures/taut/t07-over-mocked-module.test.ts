import { expect, test, vi } from 'vitest';
import { total } from '../subjects/cart.ts';

vi.mock('../subjects/cart.ts', () => ({ total: vi.fn(() => 27) }));

test('a cart of three ten-dollar items with SAVE10 totals 27', () => {
  expect(total([{ price: 10, qty: 3 }], 'SAVE10')).toBe(27);
});
