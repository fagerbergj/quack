import assert from 'node:assert/strict';
import { test } from 'node:test';
import { factsOf, type Evidence } from '../src/index.ts';

test('factsOf reads the first facts unit as the type the rubric declared', () => {
  const ev = { facts: [{ state: { facts: { n: 2 } } }] } as unknown as Evidence;
  assert.equal(factsOf<{ n: number }>(ev).n, 2);
});
