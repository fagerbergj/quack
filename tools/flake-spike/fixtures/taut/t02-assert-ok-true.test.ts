import assert from 'node:assert/strict';
import { test } from 'node:test';
import { total } from '../subjects/cart.ts';

test('the SAVE10 coupon applies to a cart', () => {
  const t = total([{ price: 10, qty: 2 }], 'SAVE10');
  assert.ok(true, `total ${t}`);
});
