import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { changedRanges, changedSince, lintSource } from './sloplint.ts';

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

test('a comment run is flagged wherever it sits: before a closing brace, at the end of a file, or trailing code lines', () => {
  const run = '  // a\n  // b\n  // c\n';
  const at = (src: string, path = 'a.ts') => lintSource(path, src, 'all').map((f) => f.line);
  assert.deepEqual(at(`function f() {\n  g();\n${run}}\n`), [3]);
  assert.deepEqual(at(`const o = {\n  a: 1,\n${run}};\n`), [3]);
  assert.deepEqual(at(`switch (x) {\n  case 1:\n${run}}\n`), [3]);
  assert.deepEqual(at(`const a = 1;\n${run}`), [2]);
  assert.deepEqual(at('const a = 1; // a\nconst b = 2; // b\nconst c = 3; // c\n'), [1]);
});

test('a comment inside code and slashes in JSX text are not comment lines', () => {
  assert.deepEqual(lintSource('a.ts', 'type T = {\n  /** a */\n  x: { /** b */ y: 1 };\n  /** c */\n};\n', 'all'), []);
  assert.deepEqual(lintSource('a.tsx', 'const x = <p>\n  // a\n  // b\n  // c\n</p>;\n', 'all'), []);
});

test('parses -U0 hunks, including pure deletions', () => {
  const diff = ['diff --git a/pkg/x.ts b/pkg/x.ts', '--- a/pkg/x.ts', '+++ b/pkg/x.ts', '@@ -1 +1,3 @@', '@@ -9,2 +11,0 @@', '@@ -20 +22 @@'].join('\n');
  assert.deepEqual(changedRanges(diff).get('pkg/x.ts'), [[1, 3], [22, 22]]);
});

test('a moved file is gated on its edited lines only, even with diff.renames off', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'quack-sloplint-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const env = { ...process.env, GIT_AUTHOR_NAME: 't', GIT_AUTHOR_EMAIL: 't@t', GIT_COMMITTER_NAME: 't', GIT_COMMITTER_EMAIL: 't@t', GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' };
  const git = (...args: string[]) => execFileSync('git', args, { cwd: dir, env, encoding: 'utf8' });
  const lines = Array.from({ length: 20 }, (_, i) => `export const v${i} = ${i};`);
  git('init', '-q');
  git('config', 'diff.renames', 'false');
  writeFileSync(join(dir, 'a.ts'), `${lines.join('\n')}\n`);
  writeFileSync(join(dir, 'gone.ts'), 'export const gone = 1;\n');
  git('add', '.');
  git('commit', '-qm', 'base');
  mkdirSync(join(dir, 'pkg'));
  git('mv', 'a.ts', 'pkg/a.ts');
  git('rm', '-q', 'gone.ts');
  writeFileSync(join(dir, 'pkg/a.ts'), `${lines.with(4, 'export const v4 = 40;').join('\n')}\n`);
  git('commit', '-qam', 'move');
  assert.deepEqual([...changedSince(dir, 'HEAD~1')], [['pkg/a.ts', [[5, 5]]]]);
});
