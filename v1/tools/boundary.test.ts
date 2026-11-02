import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, readdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { loadManifests } from '../packages/runtime/src/catalog.ts';
import { arrowProblem, boundaryProblems, LAYERS, packageEdges } from './boundary.ts';

const packages = join(import.meta.dirname, '..', 'packages');

test('no package imports upward or sideways', () => {
  assert.deepEqual(boundaryProblems(packageEdges(packages)), []);
});

test('every package has a layer', () => {
  assert.deepEqual(readdirSync(packages).filter((p) => !LAYERS[p]), []);
});

test('a manifest requires or marks optional only packages it depends on', async () => {
  const edges = packageEdges(packages);
  for (const m of await loadManifests()) {
    if (!LAYERS[m.name]) continue;
    for (const n of [...(m.requires ?? []), ...(m.optional ?? [])]) assert.ok(edges.some((e) => e.from === m.name && e.to === n), `${m.name} names ${n} in its manifest but does not depend on @quack/${n}`);
  }
});

test('arrows point down only; runtime assembles everything and only core and leaves import it', () => {
  const bad = [['github', 'sleeper'], ['judge', 'github'], ['workflows', 'sleeper'], ['sdk', 'judge'], ['index', 'workflows'], ['github', 'runtime'], ['judge', 'runtime'], ['github', 'eval'], ['github', 'nope']];
  for (const [from, to] of bad) assert.ok(arrowProblem(from, to), `${from} -> ${to}`);
  const good = [['sleeper', 'workflows'], ['workflows', 'judge'], ['github', 'sdk'], ['runtime', 'github'], ['eval', 'runtime'], ['distributor', 'runtime'], ['eval', 'judge'], ['session', 'sdk']];
  for (const [from, to] of good) assert.equal(arrowProblem(from, to), undefined, `${from} -> ${to}`);
});

test('a src import or dependency that breaks a layer is reported, and the allow list can excuse it once', () => {
  const dir = mkdtempSync(join(tmpdir(), 'quack-boundary-'));
  for (const [name, deps, src] of [['github', {}, "import { x } from '@quack/sleeper';"], ['sleeper', { '@quack/github': '*' }, '']] as const) {
    mkdirSync(join(dir, name, 'src'), { recursive: true });
    writeFileSync(join(dir, name, 'package.json'), JSON.stringify({ dependencies: deps }));
    writeFileSync(join(dir, name, 'src', 'a.ts'), src);
  }
  const edges = packageEdges(dir);
  assert.equal(boundaryProblems(edges, {}).length, 2);
  assert.deepEqual(boundaryProblems(edges, { 'github -> sleeper': '#1', 'sleeper -> github': '#1' }), []);
  assert.match(boundaryProblems([], { 'github -> sleeper': '#1' })[0], /no longer a violation/);
});
