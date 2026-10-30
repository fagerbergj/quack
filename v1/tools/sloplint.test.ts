import assert from 'node:assert/strict';
import { test } from 'node:test';
import { changedRanges, lintSource } from './sloplint.ts';

const branches = (n: number) => Array.from({ length: n }, (_, i) => `  if (x === ${i}) return ${i};`).join('\n');

test('flags a function over CC 15 only when its body is touched', () => {
  const src = `export function big(x: number) {\n${branches(15)}\n  return -1;\n}\n`;
  assert.match(lintSource('a.ts', src, 'all')[0].message, /big CC 16 > 15/);
  assert.deepEqual(lintSource('a.ts', src, [[30, 40]]), []);
  assert.deepEqual(lintSource('a.ts', `function ok(x: number) {\n${branches(14)}\n}\n`, 'all'), []);
});

test('counts && || ?? and ternaries but not nested functions', () => {
  const src = `function f(a: any) {\n  const g = () => { if (a) {} if (a) {} };\n${branches(12)}\n  return (a && a || a) ?? (a ? 1 : 2);\n}\n`;
  assert.match(lintSource('a.ts', src, 'all')[0].message, /f CC 17/);
});

test('a cc-allow comment with a reason exempts the function; without a reason it does not', () => {
  const body = `function big(x: number) {\n${branches(15)}\n}\n`;
  assert.deepEqual(lintSource('a.ts', `// sloplint: cc-allow one case per wire type\n${body}`, 'all'), []);
  assert.equal(lintSource('a.ts', `// sloplint: cc-allow\n${body}`, 'all').length, 1);
});

test('flags comment runs over two lines that touch changed lines', () => {
  const src = '// one\n// two\n// three\nconst a = 1;\n// fine\n// fine\nconst b = 2;\n/*\n block\n*/\nconst c = 3;\n';
  const lines = lintSource('a.ts', src, 'all').map((f) => f.line);
  assert.deepEqual(lines, [1, 8]);
  assert.deepEqual(lintSource('a.ts', src, [[4, 7]]), []);
});

test('parses -U0 hunks, including pure deletions', () => {
  const diff = ['diff --git a/v1/x.ts b/v1/x.ts', '--- a/v1/x.ts', '+++ b/v1/x.ts', '@@ -1 +1,3 @@', '@@ -9,2 +11,0 @@', '@@ -20 +22 @@'].join('\n');
  assert.deepEqual(changedRanges(diff).get('v1/x.ts'), [[1, 3], [22, 22]]);
});
