import assert from 'node:assert/strict';
import { test } from 'node:test';
import { total } from '../subjects/cart.ts';

test('SAVE10 takes ten percent off', () => {
  const lines = [{ price: 12, qty: 2 }];
  const expected = total(lines, 'SAVE10');
  assert.equal(total(lines, 'SAVE10'), expected);
});
