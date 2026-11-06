import { expect, test } from 'vitest';
import { same } from './same.ts';

test('same compares JSON shapes by value', () => {
  expect(same({ a: [1, { b: 'x' }] }, { a: [1, { b: 'x' }] })).toBe(true);
  expect(same({ a: [1, { b: 'x' }] }, { a: [1, { b: 'y' }] })).toBe(false);
  expect(same({ a: 1 }, { a: 1, b: undefined })).toBe(false);
  expect(same([1], { 0: 1 })).toBe(false);
  expect(same(null, {})).toBe(false);
});
