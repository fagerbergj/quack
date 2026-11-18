import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { PASS_ONLY, type AggregateDecl, type CheckDecl, type Declaration } from '../src/declaration.ts';
import { judged, registerSelector, registerSplitter, rejudge, type JudgeOptions, type Rubric } from '../src/index.ts';
import { sessionApi } from './fake-session.ts';

// Each pair is [item, group, evidence?]: the item's piece is its group's claim, the evidence goes on its shared context.
registerSplitter('test.grouped.pairs', (inputs) => (JSON.parse(inputs.pairs) as Array<[string, string, string?]>).map(([id, g, ev]) => ({
  id, text: JSON.stringify({ id, claim: `claim ${g}` }), meta: { g, ...(ev === undefined ? {} : { chunk: ev }), ...(id.startsWith('drop') ? { drop: true } : {}) },
})));
registerSelector('test.grouped.keep', (_i, c) => (c.meta?.drop ? undefined : { value: '', matches: 0 }));

const said = (choice: 'yes' | 'no', reason = `the judge says ${choice}`) => JSON.stringify({ choice, confidence: 0.9, reason });
const grouped = (inner: 'any' | 'all', outer: NonNullable<AggregateDecl['outer']>, more: Partial<AggregateDecl> = {}): AggregateDecl => ({ name: 'grouped', groupBy: 'g', inner, outer, ...more });
const declOf = (aggregate: AggregateDecl, more: Partial<CheckDecl> = {}): Declaration => ({
  scopes: { rows: { split: { name: 'test.grouped.pairs', from: 'pairs' }, piece: 'row', context: { keep: { name: 'test.grouped.keep' } } } },
  checks: { kept: { scope: 'rows', question: { type: 'bool', instructions: 'Does this chunk deliver the promise?' }, aggregate, ...more } },
});
const rubricOf = (weight = 2): Rubric => ({ name: 'r', version: 1, threshold: 0.5, criteria: [{ id: 'c', checks: [{ id: 'kept', label: 'kept', weight }] }] });
const optsOf = (declaration: Declaration, pairs: Array<[string, string, string?]>, rubric = rubricOf()): JudgeOptions => ({
  kind: 'k', rubric, declaration, inputs: async () => ({ pairs: JSON.stringify(pairs) }), maxRevisions: 2, reuse: true,
  decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 }, judgeModel: { provider: 'judge', modelId: 'j' },
});
const round = (api: never, opts: JudgeOptions, version = 1) => rejudge(api, opts, { kind: 'k', version, hash: '', content: 'x', sealedAt: 0, verdict: null }, `m${version}`, BACKGROUND_CONTEXT);
// Clef holds an item whose id ends in "t" and fails one ending in "f"; anything else it is unsure of.
const clef = (row: string) => (row.endsWith('t') ? 0.95 : row.endsWith('f') ? 0.05 : 0.5);
// Groups A (one holds, one fails), B (both hold), C (both fail).
const abc: Array<[string, string]> = [['a1t', 'A'], ['a2f', 'A'], ['b1t', 'B'], ['b2t', 'B'], ['c1f', 'C'], ['c2f', 'C']];

test('a grouped declaration without groupBy, inner or outer, or a flat one with them, is rejected at registration naming the check', () => {
  const how = (aggregate: AggregateDecl) => ({ rubric: rubricOf(), declaration: declOf(aggregate), inputs: async () => ({}) });
  assert.throws(() => judged('k', how({ name: 'grouped', groupBy: 'g', inner: 'any' })), /a grouped fold needs groupBy, inner and outer.*: kept/);
  assert.throws(() => judged('k', how({ name: 'share', inner: 'any' })), /only a grouped fold takes them: kept/);
  const torn = declOf(grouped('any', 'all'));
  torn.checks.other = { ...torn.checks.kept, aggregate: grouped('any', 'all', { groupBy: 'h' }) };
  assert.throws(() => judged('k', { ...how(grouped('any', 'all')), declaration: torn, rubric: { ...rubricOf(), criteria: [{ id: 'c', checks: [{ id: 'kept', label: 'kept', weight: 2 }, { id: 'other', label: 'other', weight: 1 }] }] } }), /group by different fields: other/);
});

test('inner and outer folds combine: each group folds its items, the outer rule folds the groups', async () => {
  const cases: Array<[AggregateDecl, boolean | number, string[]]> = [
    [grouped('any', 'all'), false, ['C']],
    [grouped('all', 'any'), true, ['A', 'C']],
    [grouped('any', 'share'), 2 / 3, ['C']],
    [grouped('all', 'atLeast', { k: 2 }), 1 / 3, ['A', 'C']],
  ];
  for (const [aggregate, met, failing] of cases) {
    const v = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(aggregate), abc));
    const k = v.checks[0];
    assert.deepEqual([k.met, k.units, k.marks, k.notes.map((n) => n.split(':')[0])], [met, 6, failing, failing], `${aggregate.inner} then ${aggregate.outer}`);
  }
  const pitfall = (await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('all', 'share')), abc, rubricOf(-2)))).checks[0];
  assert.equal(pitfall.met, 2 / 3, "a pitfall's share is of the groups where the claim failed");
});

test("a failing group's note names the group once, with the reason of its first failing item, and a reused one says when it was judged", async () => {
  const { api } = sessionApi(clef, () => said('no'));
  const opts = optsOf(declOf(grouped('any', 'all')), abc);
  assert.deepEqual((await round(api, opts, 1)).checks[0].notes, ['C: flagged by clef (no reason given)']);
  assert.deepEqual((await round(api, opts, 2)).checks[0].notes, ['C: flagged by clef (no reason given) (judged on v1)']);
});

test('a group of one item takes that item\'s answer, and a group whose every item dropped is no group at all', async () => {
  const v = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('all', 'all')), [['a1t', 'A'], ['b1f', 'B']]));
  assert.deepEqual([v.checks[0].met, v.checks[0].notes], [false, ['B: flagged by clef (no reason given)']]);
  const dropped = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('any', 'all')), [['a1t', 'A'], ['a2f', 'A'], ['drop1f', 'E']]));
  assert.deepEqual([dropped.checks[0].met, dropped.checks[0].units, dropped.checks[0].notes], [true, 2, []]);
  const empty = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('any', 'all')), []));
  assert.deepEqual([empty.checks[0].met, empty.checks[0].units], [null, 0], 'no item, no group: the check does not count');
});

test('an item without its group key is an extension error naming the item', async () => {
  const decl = declOf(grouped('any', 'all', { groupBy: 'missing' }));
  await assert.rejects(round(sessionApi(clef, () => said('no')).api, optsOf(decl, abc)), /item a1t has no group key missing/);
});

test("a decided group's unsure items are not escalated, and an undecided group is listed once the check decides", async () => {
  // One request each: A decides true on a1t, so its unsure a2 is never escalated; under outer any, B's pass decides the check, so C stays open.
  const { api, seen } = sessionApi(clef, () => said('yes'));
  const v = await round(api, optsOf(declOf(grouped('any', 'all')), [['a1t', 'A', 'x'], ['a2', 'A', 'x'], ['b1t', 'B']]));
  assert.deepEqual([v.checks[0].met, seen.judge], [true, []]);
  const { api: api2, seen: seen2 } = sessionApi(clef, () => said('yes'));
  const w = await round(api2, optsOf(declOf(grouped('all', 'any')), [['b1t', 'B', 'x'], ['c1', 'C', 'x']]));
  assert.deepEqual([w.checks[0].met, w.checks[0].undecidedItems, w.checks[0].undecided, seen2.judge], [true, ['C'], undefined, []]);
});

test("under pass-only a group's failing decision-model answer goes to the judge model, whose answer and reason stand, unless its group already decided", async () => {
  const { api, seen } = sessionApi(clef, (row) => (row === 'b1f' ? said('no', 'b1 never sets the header') : said('yes')));
  // a1t and a2f go in one request, so A is decided before a2f could escalate; outer share waits for every group.
  const v = await round(api, optsOf(declOf(grouped('any', 'share'), { decisionModel: PASS_ONLY }), [['a1t', 'A', 'x'], ['a2f', 'A', 'x'], ['b1f', 'B', 'y'], ['c1f', 'C', 'z']]));
  const k = v.checks[0];
  assert.deepEqual(seen.judge.sort(), ['b1f', 'c1f'], 'a2f is not escalated: a1t already decided A');
  assert.deepEqual([k.met, k.notes, k.resolvers], [2 / 3, ['B: b1 never sets the header'], { clef: 1, j: 2 }], "c1f's flag was overturned by the judge model");
});

test('reuse inside a group: an unchanged item is reused, a changed evidence or group claim asks again', async () => {
  const { api, seen } = sessionApi(clef, () => said('no'));
  const pairs: Array<[string, string, string]> = [['a1t', 'A', 'x'], ['a2f', 'A', 'y'], ['b1t', 'B', 'x']];
  const v1 = await round(api, optsOf(declOf(grouped('any', 'share')), pairs), 1);
  assert.equal(v1.checks[0].met, 1);
  const v2 = await round(api, optsOf(declOf(grouped('any', 'share')), pairs), 2);
  assert.deepEqual([seen.clef.length, v2.checks[0].reused, v2.checks[0].resolvers], [3, { 'rows a1t': 1, 'rows a2f': 1, 'rows b1t': 1 }, {}]);
  const moved: Array<[string, string, string]> = [['a1t', 'A', 'x'], ['a2f', 'A', 'z'], ['b1t', 'C', 'x']];
  const v3 = await round(api, optsOf(declOf(grouped('any', 'share')), moved), 3);
  assert.deepEqual([seen.clef.slice(3).sort(), v3.checks[0].reused], [['a2f', 'b1t'], { 'rows a1t': 1 }], "a2f's evidence and b1t's group claim changed");
});
