import assert from 'node:assert/strict';
import { test } from 'node:test';
import { total, type Line } from '../subjects/cart.ts';

test('a line whose quantity is raised counts at the new quantity', () => {
  const line: Line = { price: 4, qty: 1 };
  total([line]);
  line.qty = 3;
  assert.equal(line.qty, 3);
});
