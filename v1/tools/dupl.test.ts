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
  const rows = (n: number) => `function f() {\n  const n = [${Array.from({ length: n }, () => '0').join(', ')}];\n  return n;\n}\n`;
  assert.deepEqual(clones(new Map([['a.ts', rows(100)]]), new Map([['a.ts', 'all']])), []);
  const unit = Array.from({ length: 12 }, (_, i) => `x${i}: ${i % 3}`).join(', ');
  const table = `function g() {\n  return [${Array.from({ length: 8 }, () => `{ ${unit} }`).join(',\n    ')}];\n}\n`;
  assert.deepEqual(clones(new Map([['a.ts', table], ['b.ts', table]]), new Map([['a.ts', 'all']])).map((c) => c.other), ['b.ts'], 'another file holding the table is a copy');
});

test('a block pasted twice in one file is still a copy', () => {
  const src = `${fn('evens', 'xs')}${fn('odds', 'ys')}`;
  assert.deepEqual(clones(new Map([['a.ts', src]]), new Map([['a.ts', [[10, 20]]]])).map((c) => [c.line, c.other, c.otherLine]), [[10, 'a.ts', 1]]);
});
