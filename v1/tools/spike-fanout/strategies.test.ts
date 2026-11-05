import assert from 'node:assert/strict';
import { test } from 'node:test';
import { chunkers, fold, unitsOf, type Aggregate } from './strategies.ts';

const diff = 'diff --git a/a.ts b/a.ts\nnew file mode 100644\n--- /dev/null\n+++ b/a.ts\n@@ -0,0 +1 @@\n+x\ndiff --git a/b.ts b/b.ts\n--- a/b.ts\n+++ b/b.ts\n@@ -1 +1 @@\n-y\n+z\n@@ -9 +9 @@\n-p\n+q\n';

test('structure chunkers split on their own markers', () => {
  assert.deepEqual(chunkers.diffFiles({ d: diff }, { from: 'd' }).map((c) => c.id), ['a.ts', 'b.ts']);
  assert.deepEqual(chunkers.diffFiles({ d: diff }, { from: 'd', filter: 'added' }).map((c) => c.id), ['a.ts']);
  assert.equal(chunkers.diffHunks({ d: diff }, { from: 'd' }).length, 3);
  assert.deepEqual(chunkers.listItems({ r: '## F\n- a\n  more\n- b\n## G\n- c' }, { from: 'r', sections: ['F'] }).map((c) => c.text), ['a more', 'b']);
  assert.equal(chunkers.openapiOperations({ o: JSON.stringify({ paths: { '/x': { get: {}, post: {}, parameters: [] } } }) }, { from: 'o' }).length, 2);
  assert.deepEqual(chunkers.schemaProperties({ s: JSON.stringify({ properties: { a: {}, b: {} } }) }, { from: 's' }).map((c) => c.id), ['a', 'b']);
});

test('aggregators are three-valued: all, any, share and coverageUnion decide only when unknown chunks cannot change the result', () => {
  const yes = { answer: { p: 0.95 } };
  const no = { answer: { p: 0.05 } };
  const maybe = { answer: { p: 0.5 } };
  const run = (name: string, items: object[], rest: Partial<Aggregate> = {}) => fold({ name, ...rest }, items).value;
  assert.equal(run('all', [yes, maybe]), undefined);
  assert.equal(run('all', [no, maybe]), false);
  assert.equal(run('any', [yes, maybe]), true);
  assert.equal(run('any', [no, no]), false);
  assert.equal(run('share', [yes, no, maybe], { min: 0.5 }), undefined);
  assert.equal(run('share', [yes, yes, no], { min: 0.5 }), true);
  assert.equal(run('coverageUnion', [{ related: 1 }, yes], { by: 'either' }), true);
  assert.equal(run('coverageUnion', [{ related: 1 }, no], { by: 'either' }), false);
  assert.equal(run('all', []), true);
});

test('a model split drops items whose span is not in the input, and a deterministic splitter falls back when its shape is off', () => {
  const inputs = { text: 'Add x. Fix y.', 'llm:t': JSON.stringify([{ item: 'x', span: 'Add x' }, { item: 'made up', span: 'Remove z' }]) };
  const model = { name: 'llm', from: 'text', recorded: 'llm:t' };
  const got = unitsOf({ split: model }, inputs);
  assert.deepEqual(got.units.map((u) => u.id), ['i1']);
  assert.equal(got.rejected, 1);
  assert.equal(unitsOf({ split: { ...model, sweep: { name: 'sentences' } } }, inputs).swept, 1);
  const auto = unitsOf({ split: { name: 'sentences', from: 'text', expect: { max: 1 }, fallback: model } }, inputs);
  assert.equal(auto.splitter, 'sentences -> llm');
  assert.equal(auto.reason, 'more than 1 items');
});
