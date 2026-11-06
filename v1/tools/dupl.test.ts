import assert from 'node:assert/strict';
import { test } from 'node:test';
import { clones } from './dupl.ts';

const fn = (name: string, v: string) => `export function ${name}(${v}: number[]) {
  const out: number[] = [];
  for (const x of ${v}) {
    if (x > 0 && x % 2 === 0) out.push(x * 2);
    else if (x < 0) out.push(-x);
    else out.push(0);
  }
  return out.filter((y) => y !== 0).map((y) => y + 1).sort((a, b) => a - b);
}
`;

test('a copy with renamed names in a changed range is reported against the original, once', () => {
  const sources = new Map([['a.ts', fn('evens', 'xs')], ['b.ts', `const pad = 1;\n${fn('doubled', 'values')}`]]);
  const found = clones(sources, new Map([['b.ts', [[2, 4]]]]));
  assert.deepEqual(found.map((c) => [c.file, c.line, c.other, c.otherLine]), [['b.ts', 2, 'a.ts', 1]]);
});

test('only changed lines are gated, and short or merely similar code is not a clone', () => {
  const sources = new Map([['a.ts', fn('evens', 'xs')], ['b.ts', `const pad = 1;\n${fn('doubled', 'values')}`]]);
  assert.deepEqual(clones(sources, new Map([['b.ts', [[1, 1]]]])), []);
  const short = new Map([['a.ts', 'export const f = (x: number) => x + 1;\n'], ['b.ts', 'export const g = (y: number) => y + 1;\n']]);
  assert.deepEqual(clones(short, new Map([['b.ts', 'all']])), []);
  const table = (s: string) => `export const rubric = [${Array.from({ length: 12 }, (_, i) => `{ id: '${s}${i}', weight: ${i} }`).join(', ')}];\n`;
  assert.deepEqual(clones(new Map([['a.ts', table('lineup')], ['b.ts', table('trades')]]), new Map([['b.ts', 'all']])), []);
});

test('a pattern repeating inside one stretch of a file is not a copy of itself', () => {
  const rows = `export const n = [${Array.from({ length: 50 }, () => '0').join(', ')}];\n`;
  assert.deepEqual(clones(new Map([['a.ts', rows]]), new Map([['a.ts', 'all']])), []);
});
