import assert from 'node:assert/strict';
import { test } from 'node:test';
import { total } from '../subjects/cart.ts';

test('a discounted cart never totals below zero', () => {
  assert.ok(total([{ price: 19.99, qty: 2 }], 'SAVE10') >= 0);
  assert.ok(total([], 'SAVE10') >= 0);
});
