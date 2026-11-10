import assert from 'node:assert/strict';
import { test } from 'node:test';
import { total } from '../subjects/cart.ts';

test('totals: lines sum, returned lines count nothing, SAVE10 is 10% off, cents round', () => {
  assert.equal(total([{ price: 10, qty: 3 }]), 30);
  assert.equal(total([{ price: 10, qty: 3 }, { price: 99, qty: 0 }, { price: 5, qty: -2 }]), 30);
  assert.equal(total([{ price: 10, qty: 3 }], 'SAVE10'), 27);
  assert.equal(total([{ price: 10, qty: 3 }], 'SAVE20'), 30);
  assert.equal(total([{ price: 0.333, qty: 1 }]), 0.33);
  assert.equal(total([]), 0);
});
