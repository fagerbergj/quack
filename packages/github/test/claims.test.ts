import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { diffClaims, namedCode } from '../src/claims.ts';

const facts = (changed: Record<string, string>, tree: string[]) => ({ changed: new Map(Object.entries(changed)), paths: [...new Set([...tree, ...Object.keys(changed)])] });
const d = facts({ 'src/app.ts': 'M', 'src/nav.ts': 'A', 'src/old.ts': 'D', 'test/app.test.ts': 'M' }, ['src/app.ts', 'src/nav.ts', 'test/app.test.ts', 'docs/api.md', 'README.md']);

test('a named path under a top directory of the repo must be in the diff or the tree; another repo or the workspace is not checked', () => {
  const text = '# t\n\nChanged `src/app.ts:12` and `src/apps.ts`; see `docs/` and `artifacts/spec.json`, and `cmd/mock/main.go` in the other repo.';
  assert.deepEqual(diffClaims(text, d, new Set()), ['artifacts/pr.md names `src/apps.ts`, which is in neither the diff nor the repository']);
});

test('a code name counts only where the text says something was added, and a name the trees hold passes', () => {
  const text = '# t\n\n- Added `parseLimit()` and `readConfig`.\n- `TaskGraphView` in the dependency never writes.\n\nThe new `Limits.perReview` field.';
  assert.deepEqual(namedCode(text), ['parseLimit', 'readConfig', 'TaskGraphView', 'perReview']);
  assert.deepEqual(diffClaims(text, d, new Set(['readConfig'])), ['parseLimit', 'perReview'].map((n) => `artifacts/pr.md names \`${n}\`, which is in neither the diff nor the repository`));
});

test('a file said to be new, deleted, changed or unchanged must match what the diff did to it', () => {
  const right = 'New `src/nav.ts`; `src/old.ts` deleted; updated `app.test.ts`; `docs/api.md` unchanged.';
  assert.deepEqual(diffClaims(right, d, new Set()), []);
  const wrong = 'New `src/app.ts`; deleted `src/nav.ts`; edited `docs/api.md`; no change to `src/app.ts`.';
  assert.deepEqual(diffClaims(wrong, d, new Set()), [
    'artifacts/pr.md says `src/app.ts` is new, but the diff does not add it',
    'artifacts/pr.md says `src/nav.ts` was deleted, but the diff does not delete it',
    'artifacts/pr.md says `docs/api.md` changed, but the diff does not touch it',
    'artifacts/pr.md says `src/app.ts` is unchanged, but the diff touches it',
  ]);
});

test('a docs-only or no-Go claim fails on a diff that breaks it, and holds on one that keeps it', () => {
  const docs = facts({ 'docs/api.md': 'M' }, ['docs/api.md']);
  assert.deepEqual(diffClaims('Docs-only change, no Go code changes.', docs, new Set()), []);
  const code = facts({ 'docs/api.md': 'M', 'internal/serve.go': 'M' }, []);
  assert.deepEqual(diffClaims('Docs-only change, no Go code changes.', code, new Set()), [
    'artifacts/pr.md says the change is docs-only, but the diff touches internal/serve.go',
    'artifacts/pr.md says there are no Go changes, but the diff touches internal/serve.go',
  ]);
});

test('fenced blocks, commands, commit ids, plain words and bare file names are not names', () => {
  const text = 'Added `npm test`, `deadbeef1`, `done`, `pass-only` and `knip.json`.\n\n```\nAdded `src/ghost.ts` and `ghostFn`\n```\n';
  assert.deepEqual(namedCode(text), []);
  assert.deepEqual(diffClaims(text, d, new Set()), []);
});

test('a root file such as a Makefile is a file in the tree, not a code name to grep for', () => {
  assert.deepEqual(diffClaims('Added a `Makefile` target.', facts({ Makefile: 'A' }, []), new Set()), []);
});

test('a name cited in a sentence that adds nothing is not checked, even beside one that does', () => {
  assert.deepEqual(diffClaims('- Added the panel; it renders `TaskGraphView` from the plugin.', d, new Set()), []);
});

test('a quoted phrase is a mention, not a claim', () => {
  const text = 'The gate refuses a body that says "docs-only" while touching code, or "New `src/app.ts`" for a modified file.';
  assert.deepEqual(diffClaims(text, d, new Set()), []);
});

test('a scope phrase that is reported, backticked or a bare term is a mention, not a claim about this change', () => {
  const code = facts({ 'src/app.ts': 'M' }, []);
  const text = 'Fails when pr.md claims the change is docs-only while touching code, or calls itself a docs-only change.\nThe gate reads `docs-only` and the docs-only rule.\nIt says there are no TypeScript changes, but the diff touches src/app.ts.';
  assert.deepEqual(diffClaims(text, code, new Set()), []);
  assert.deepEqual(diffClaims('This PR is docs-only.', code, new Set()), ['artifacts/pr.md says the change is docs-only, but the diff touches src/app.ts']);
});

test('docs-only allows doc assets under docs/', () => {
  assert.deepEqual(diffClaims('Docs-only change.', facts({ 'docs/api.md': 'M', 'docs/diagram.svg': 'A' }, []), new Set()), []);
});

test('without a name lookup no code name is judged, and paths still are', () => {
  assert.deepEqual(diffClaims('Added `parseLimit()` in `src/apps.ts`.', d, undefined), ['artifacts/pr.md names `src/apps.ts`, which is in neither the diff nor the repository']);
});

type Case = { id: string; base?: string; text?: string; find?: string; replace?: string; append?: string; falseClaims: string[]; changed: Record<string, string>; paths: string[]; found: string[]; flags: string[] };
const CORPUS = JSON.parse(readFileSync(new URL('../../../test-fixtures/claims/corpus.json', import.meta.url), 'utf8')) as Case[];

// Real PR bodies, labelled by hand against their diffs, plus marked edits of them; each keeps the tree facts its spans reach.
test('the labelled corpus: no flag where every claim is true, and each recorded catch still fires', () => {
  const texts = new Map(CORPUS.map((c) => [c.id, c.text]));
  for (const c of CORPUS) {
    const base = texts.get(c.base ?? c.id)!;
    const text = c.text ?? (c.append ? `${base}\n\n${c.append}` : base.replace(c.find!, c.replace!));
    const flags = diffClaims(text, { changed: new Map(Object.entries(c.changed)), paths: c.paths }, new Set(c.found));
    assert.deepEqual(flags, c.falseClaims.length ? c.flags : [], c.id);
  }
});
