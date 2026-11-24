import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { baseSources, cycleFindings, importEdges } from './cycles.ts';

const P = 'packages/p/src';
const src = (files: Record<string, string>) => new Map(Object.entries(files).map(([f, text]) => [`${P}/${f}`, text]));
const gate = (base: Record<string, string>, head: Record<string, string>) => cycleFindings(importEdges(src(base)), importEdges(src(head)));
const A = { 'a.ts': "import { b } from './b.ts';\nexport const a = () => b;\n", 'b.ts': 'export const b = 1;\n' };

test('a new runtime cycle fails at the import that closes it, with the way round', () => {
  const found = gate(A, { ...A, 'b.ts': "import { a } from './a.ts';\nexport const b = a;\n" });
  assert.deepEqual(found.map((f) => [f.file, f.line]), [[`${P}/b.ts`, 1]]);
  assert.match(found[0].message, new RegExp(`across 2 files: ${P}/b.ts:1 → ${P}/a.ts, ${P}/a.ts:1 → ${P}/b.ts$`));
});

test('a type-only back edge passes', () => {
  assert.deepEqual(gate(A, { ...A, 'b.ts': "import type { a } from './a.ts';\nimport { type a as c } from './a.ts';\nexport const b = 1;\n" }), []);
});

test('a barrel re-export passes', () => {
  const head = { 'index.ts': "export { m } from './m.ts';\nexport * from './n.ts';\n", 'm.ts': "import { n } from './index.ts';\nexport const m = n;\n", 'n.ts': 'export const n = 1;\n' };
  assert.deepEqual(gate({}, head), []);
});

const CYCLE = { ...A, 'b.ts': "import { a } from './a.ts';\nexport const b = a;\n" };

test('an existing cycle the diff leaves alone passes, even when the diff imports elsewhere', () => {
  assert.deepEqual(gate(CYCLE, { ...CYCLE, 'c.ts': "import { a } from './a.ts';\nexport const c = a;\n" }), []);
});

test('a diff that adds an edge to an existing cycle fails', () => {
  const head = { ...CYCLE, 'a.ts': "import { b } from './b.ts';\nimport { c } from './c.ts';\nexport const a = () => b + c;\n", 'c.ts': "import { b } from './b.ts';\nexport const c = b;\n" };
  assert.deepEqual(gate(CYCLE, head).map((f) => [f.file, f.line]), [[`${P}/a.ts`, 2]]);
});

test('directories that import each other fail though no two files do', () => {
  const head = { 'deps.ts': "import { news } from './lineup/news.ts';\nexport const deps = news;\n", 'lineup/news.ts': 'export const news = 1;\n', 'lineup/index.ts': "import { deps } from '../deps.ts';\nexport const run = deps;\n" };
  const found = gate({}, head);
  assert.deepEqual(found.map((f) => [f.file, f.line]), [[`${P}/deps.ts`, 1]]);
  assert.match(found[0].message, new RegExp(`across 2 entries of ${P}/: ${P}/deps.ts:1 → ${P}/lineup/news.ts, ${P}/lineup/index.ts:1 → ${P}/deps.ts$`));
});

test('the base reads back the files the diff changed, added or deleted at the merge base', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'quack-cycles-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const env = { ...process.env, GIT_AUTHOR_NAME: 't', GIT_AUTHOR_EMAIL: 't@t', GIT_COMMITTER_NAME: 't', GIT_COMMITTER_EMAIL: 't@t', GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' };
  const git = (...args: string[]) => execFileSync('git', args, { cwd: dir, env, encoding: 'utf8' });
  const write = (files: Map<string, string>) => files.forEach((text, f) => writeFileSync(join(dir, f), text));
  mkdirSync(join(dir, P), { recursive: true });
  git('init', '-q', '-b', 'main');
  write(src({ ...A, 'gone.ts': 'export const gone = 1;\n' }));
  git('add', '.');
  git('commit', '-qm', 'base');
  git('checkout', '-qb', 'change');
  git('rm', '-q', `${P}/gone.ts`);
  const head = src({ ...CYCLE, 'c.ts': 'export const c = 1;\n' });
  write(head);
  git('add', '.');
  git('commit', '-qm', 'cycle');
  const base = baseSources(dir, 'main', head, () => true);
  assert.deepEqual(base, src({ ...A, 'gone.ts': 'export const gone = 1;\n' }));
  assert.equal(cycleFindings(importEdges(base), importEdges(head)).length, 1);
});
