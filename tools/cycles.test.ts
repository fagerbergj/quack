import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { baseSources, cycleFindings } from './cycles.ts';

const P = 'packages/p/src';
const src = (files: Record<string, string>) => new Map(Object.entries(files).map(([f, text]) => [`${P}/${f}`, text]));
const gate = (base: Record<string, string>, head: Record<string, string>) => cycleFindings({ sources: src(base), moved: new Map() }, src(head));
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
  assert.equal(found[0].message, `import cycle across 2 entries of ${P}/ (deps.ts, lineup/): deps.ts:1 → lineup/, lineup/index.ts:1 → deps.ts`);
});

/** A temp git repo on `main` holding `base`, then a `change` branch where `edit` runs and commits; returns the gate's findings. */
function branch(t: { after: (f: () => void) => void }, base: Record<string, string>, edit: (git: (...a: string[]) => string, write: (f: Record<string, string>) => void) => void) {
  const dir = mkdtempSync(join(tmpdir(), 'quack-cycles-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const env = { ...process.env, GIT_AUTHOR_NAME: 't', GIT_AUTHOR_EMAIL: 't@t', GIT_COMMITTER_NAME: 't', GIT_COMMITTER_EMAIL: 't@t', GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' };
  const git = (...args: string[]) => execFileSync('git', args, { cwd: dir, env, encoding: 'utf8' });
  const write = (files: Record<string, string>) => src(files).forEach((text, f) => { mkdirSync(dirname(join(dir, f)), { recursive: true }); writeFileSync(join(dir, f), text); });
  git('init', '-q', '-b', 'main');
  write(base);
  git('add', '.');
  git('commit', '-qm', 'base');
  git('checkout', '-qb', 'change');
  edit(git, write);
  git('add', '-A');
  git('commit', '-qm', 'change');
  const head = new Map(git('ls-files').split('\n').filter(Boolean).map((f) => [f, readFileSync(join(dir, f), 'utf8')]));
  const b = baseSources(dir, 'main', head, () => true);
  return { base: b, found: cycleFindings(b, head) };
}

test('the base reads back the files the diff changed, added or deleted at the merge base', (t) => {
  const { base, found } = branch(t, { ...A, 'gone.ts': 'export const gone = 1;\n' }, (git, write) => {
    git('rm', '-q', `${P}/gone.ts`);
    write({ ...CYCLE, 'c.ts': 'export const c = 1;\n' });
  });
  assert.deepEqual(base.sources, src({ ...A, 'gone.ts': 'export const gone = 1;\n' }));
  assert.equal(found.length, 1);
});

test('moving a file inside an existing cycle, with its one import updated, passes', (t) => {
  const { found } = branch(t, CYCLE, (git, write) => {
    git('mv', `${P}/b.ts`, `${P}/renamed.ts`);
    write({ 'a.ts': "import { b } from './renamed.ts';\nexport const a = () => b;\n" });
  });
  assert.deepEqual(found, []);
});

test('renaming a directory that holds an existing cycle passes', (t) => {
  const lib = { 'lib/a.ts': CYCLE['a.ts'], 'lib/b.ts': CYCLE['b.ts'], 'main.ts': "import { a } from './lib/a.ts';\nexport const m = a;\n" };
  const { found } = branch(t, lib, (git, write) => {
    git('mv', `${P}/lib`, `${P}/core`);
    write({ 'main.ts': "import { a } from './core/a.ts';\nexport const m = a;\n" });
  });
  assert.deepEqual(found, []);
});

test('a move that also adds a back edge still fails', (t) => {
  const three = { ...CYCLE, 'c.ts': 'export const c = 1;\n' };
  const { found } = branch(t, three, (git, write) => {
    git('mv', `${P}/c.ts`, `${P}/moved.ts`);
    write({ 'moved.ts': "import { a } from './a.ts';\nexport const c = a;\n", 'b.ts': "import { a } from './a.ts';\nimport { c } from './moved.ts';\nexport const b = a + c;\n" });
  });
  assert.deepEqual(found.map((f) => [f.file, f.line]), [[`${P}/b.ts`, 2]]);
});
