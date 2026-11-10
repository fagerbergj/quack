import assert from 'node:assert/strict';
import { test } from 'node:test';
import { kindOf, mdUnits, tsUnits } from './bench/clef-lint.ts';

const span = (us: Array<{ start: number; end: number }>) => us.map((u) => [u.start, u.end]);

test('a test file chunks into each changed test case, inside its describe, and leaves unchanged cases out', () => {
  const src = ["import { test } from 'node:test';", "describe('store', () => {", "  test('a', () => {", '    one();', '  });', "  it('b', () => {", '    two();', '  });', '});'].join('\n');
  const units = tsUnits('x.test.ts', src, true, [[7, 7]]);
  assert.deepEqual(span(units), [[6, 8]]);
  assert.deepEqual(units[0].enclosing, ["'store'"]);
});

test('a changed line in a long function chunks to its nested statement, with the function head as context', () => {
  const body = Array.from({ length: 100 }, (_, i) => `  const v${i} = ${i};`);
  const src = ['export function big() {', ...body, '}', 'const a = 1;', 'const b = 2;'].join('\n');
  const units = tsUnits('x.ts', src, false, [[51, 51], [103, 104]]);
  assert.deepEqual(span(units), [[51, 51], [103, 104]]);
  assert.deepEqual(units[0].enclosing, ['export function big() {']);
});

test('markdown chunks by heading, and files map to their lint', () => {
  assert.deepEqual(span(mdUnits('intro\n# A\ntext\n## B\nmore\n```\n# not a heading\n```')), [[1, 1], [2, 3], [4, 8]]);
  assert.deepEqual(['a/test/b.ts', 'a/src/b.tsx', 'docs/x.md', 'agents/r/prompt.md', 'x.d.ts', 'fixtures-specs/s.json'].map(kindOf), ['tests', 'code', 'docs', undefined, undefined, 'spec']);
});
