import assert from 'node:assert/strict';
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

test('fenced blocks, commands, commit ids and plain words are not names', () => {
  const text = 'Added `npm test`, `deadbeef1`, `done` and `pass-only`.\n\n```\nAdded `src/ghost.ts` and `ghostFn`\n```\n';
  assert.deepEqual(namedCode(text), []);
  assert.deepEqual(diffClaims(text, d, new Set()), []);
});
