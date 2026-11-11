import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { loadManifests } from '../packages/runtime/src/catalog.ts';
import { arrowProblem, boundaryProblems, LAYERS, packageEdges } from './boundary.ts';

const packages = join(import.meta.dirname, '..', 'packages');

test('no package imports upward or sideways', () => {
  assert.deepEqual(boundaryProblems(packageEdges(packages)), []);
});

// The SDK contract makes the primer the runtime's boot hand-off: an extension that primed could swap a session's app credential.
test('only the runtime primes the session endpoint; no other package source names primeSessionEndpoint', () => {
  const users = readdirSync(packages).flatMap((p) => {
    const src = join(packages, p, 'src');
    const files = readdirSync(src, { recursive: true, withFileTypes: true }).filter((e) => e.isFile() && /\.tsx?$/.test(e.name));
    return files.some((e) => readFileSync(join(e.parentPath, e.name), 'utf8').includes('primeSessionEndpoint')) ? [p] : [];
  });
  assert.deepEqual(users.sort(), ['runtime', 'sdk'], 'sdk defines it; the runtime calls it at boot and in validate');
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
  const bad = [['github', 'sleeper'], ['judge', 'github'], ['workflows', 'sleeper'], ['sdk', 'judge'], ['catalog', 'workflows'], ['github', 'runtime'], ['judge', 'runtime'], ['github', 'eval'], ['github', 'nope'],
    ['github', 'distributor'], ['sleeper', 'session'], ['judge', 'catalog'], ['workflows', 'session'], ['research', 'web']];
  for (const [from, to] of bad) assert.ok(arrowProblem(from, to), `${from} -> ${to}`);
  const good = [['sleeper', 'workflows'], ['workflows', 'judge'], ['github', 'sdk'], ['runtime', 'github'], ['eval', 'runtime'], ['distributor', 'runtime'], ['eval', 'judge'], ['session', 'sdk']];
  for (const [from, to] of good) assert.equal(arrowProblem(from, to), undefined, `${from} -> ${to}`);
});

function github(src: string, deps: Record<string, string> = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'quack-boundary-'));
  for (const name of ['github', 'sleeper', 'sdk', 'judge']) {
    mkdirSync(join(dir, name, 'src'), { recursive: true });
    writeFileSync(join(dir, name, 'package.json'), JSON.stringify({ dependencies: name === 'github' ? deps : {} }));
  }
  writeFileSync(join(dir, 'github', 'src', 'a.tsx'), src);
  return boundaryProblems(packageEdges(dir), {});
}

test('every import form is seen: quotes, side effects, re-exports, dynamic, type, require, relative and line breaks', () => {
  const forms = [
    'import { x } from "@quack/sleeper";', "import '@quack/sleeper';", "export * from '@quack/sleeper/x';", 'const m = await import(`@quack/sleeper`);',
    'const m = await import("@quack/sleeper");', "const m = require('@quack/sleeper');", "import { x } from '../../sleeper/src/index.ts';",
    "import {\n  x,\n} from\n  '@quack/sleeper';", "type T = import('@quack/sleeper').T;", "import type { T } from '@quack/sleeper';",
  ];
  for (const src of forms) assert.match(github(src).join('\n'), /github \(feature\) must not import sleeper/, src);
  assert.match(github('const m = await import(name);').join('\n'), /computed name/);
  assert.match(github("import x from '../../../elsewhere.ts';").join('\n'), /outside packages/);
  const runtime = (to: string) => boundaryProblems([{ from: 'runtime', to, via: 'src/a.ts', declared: false }], {});
  assert.deepEqual(runtime('<computed spec>'), [], 'the runtime loads extensions by computed specifier');
  assert.match(runtime('<outside packages>').join('\n'), /runtime imports a file outside packages/);
});

test('an import of a package the importer does not declare is reported, even when its layer allows it', () => {
  assert.deepEqual(github("import { x } from '@quack/sdk';"), ['github imports @quack/sdk without declaring it in package.json (src/a.tsx)']);
  assert.deepEqual(github("import { x } from '@quack/sdk';\nimport { j } from './judge.ts';", { '@quack/sdk': '*' }), []);
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
