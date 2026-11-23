import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { gateGenerations } from '@quack/sdk';
import { PASS_ONLY, type AggregateDecl, type CheckDecl, type Declaration } from '../src/declaration.ts';
import { check, feedback, judged, registerSelector, registerSplitter, rejudge, type JudgeOptions, type Rubric } from '../src/index.ts';
import { sessionApi } from './fake-session.ts';

// Each pair is [item, group, evidence?]: the item's piece is its group's claim, the evidence goes on its shared context.
registerSplitter('test.grouped.pairs', (inputs) => (JSON.parse(inputs.pairs) as Array<[string, unknown, string?]>).map(([id, g, ev]) => ({
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
  assert.throws(() => judged('k', how({ name: 'share', inner: 'any' })), /only a grouped fold takes them.*: kept/);
  assert.throws(() => judged('k', how(grouped('all', 'all', { none: 'none held' }))), /only an inner any takes none: kept/);
  const torn = declOf(grouped('any', 'all'));
  torn.checks.other = { ...torn.checks.kept, aggregate: grouped('any', 'all', { groupBy: 'h' }) };
  assert.throws(() => judged('k', { ...how(grouped('any', 'all')), declaration: torn, rubric: { ...rubricOf(), criteria: [{ id: 'c', checks: [{ id: 'kept', label: 'kept', weight: 2 }, { id: 'other', label: 'other', weight: 1 }] }] } }), /group by different fields: other/);
});

test('inner and outer folds combine: each group folds its items, the outer rule folds the groups', async () => {
  const cases: Array<[AggregateDecl, boolean | number, string[]]> = [
    [grouped('any', 'all'), false, ['C']],
    [grouped('all', 'any'), true, ['A', 'C']],
    [grouped('any', 'share'), 2 / 3, ['C']],
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
  assert.deepEqual((await round(api, opts, 1)).checks[0].notes, ['C: rows c1f: flagged by clef (no reason given); re-verify it against the cited source before rewording it']);
  assert.deepEqual((await round(api, opts, 2)).checks[0].notes, ['C: rows c1f: flagged by clef (no reason given); re-verify it against the cited source before rewording it (judged on v1)']);
});

test("an inner any fold with `none` notes a group no item gave a reason for in code's words, counting its items", async () => {
  const v = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('any', 'all', { none: 'none of its {n} rows delivers it' })), abc));
  assert.deepEqual([v.checks[0].met, v.checks[0].notes, v.checks[0].marks], [false, ['C: none of its 2 rows delivers it'], ['C']]);
});

test("a failing group's escalated item keeps the judge model's reason over `none`", async () => {
  // Clef is unsure of both items of C, so both escalate and the judge model's reason is the note.
  const v = await round(sessionApi(clef, () => said('no', 'nothing here delivers C')).api, optsOf(declOf(grouped('any', 'all', { none: 'none of its {n} rows delivers it' })), [['a1t', 'A'], ['c1u', 'C'], ['c2u', 'C']]));
  assert.deepEqual([v.checks[0].met, v.checks[0].notes], [false, ['C: rows c1u: nothing here delivers C']]);
});

test('a group of one item takes that item\'s answer, and a group whose every item dropped is no group at all', async () => {
  const v = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('all', 'all')), [['a1t', 'A'], ['b1f', 'B']]));
  assert.deepEqual([v.checks[0].met, v.checks[0].notes], [false, ['B: rows b1f: flagged by clef (no reason given); re-verify it against the cited source before rewording it']]);
  const dropped = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('any', 'all')), [['a1t', 'A'], ['a2f', 'A'], ['drop1f', 'E']]));
  assert.deepEqual([dropped.checks[0].met, dropped.checks[0].units, dropped.checks[0].notes], [true, 2, []]);
  const empty = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('any', 'all')), []));
  assert.deepEqual([empty.checks[0].met, empty.checks[0].units], [null, 0], 'no item, no group: the check does not count');
});

test('an item without a string or number group key is an extension error naming the item', async () => {
  const decl = declOf(grouped('any', 'all', { groupBy: 'missing' }));
  await assert.rejects(round(sessionApi(clef, () => said('no')).api, optsOf(decl, abc)), /item a1t has no string or number group key missing/);
  for (const key of [null, { g: 1 }]) {
    const pairs = [['a1t', key]] as unknown as Array<[string, string]>;
    await assert.rejects(round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('any', 'all')), pairs)), /item a1t has no string or number group key g/, JSON.stringify(key));
  }
  const numbered = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('any', 'all')), [['a1f', 7]] as unknown as Array<[string, string]>));
  assert.deepEqual(numbered.checks[0].marks, ['7']);
});

test("a grouped share's feedback counts groups, not items, and only when there is more than one", async () => {
  const v = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('any', 'share')), abc));
  assert.deepEqual(feedback(v.checks), ['missed c.kept (2 of 3): C: rows c1f: flagged by clef (no reason given); re-verify it against the cited source before rewording it'], 'groups A and B held; not "4 of 6" items');
  assert.equal(v.checks[0].groups, 3);
  const one = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('any', 'share')), [['c1f', 'C'], ['c2f', 'C']]));
  assert.deepEqual([one.checks[0].groups, feedback(one.checks)], [1, ['missed c.kept: C: rows c1f: flagged by clef (no reason given); re-verify it against the cited source before rewording it']]);
});

test('groups is absent on a flat fold and when no item applies', async () => {
  const flat = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf({ name: 'share' }), abc));
  assert.equal(flat.checks[0].groups, undefined);
  const empty = await round(sessionApi(clef, () => said('no')).api, optsOf(declOf(grouped('any', 'share')), []));
  assert.equal(empty.checks[0].groups, undefined);
});

test("a judge model that errors on an open group of a share leaves the check undecided, counting only open groups' items, and attaches no verdict", async () => {
  // A decides on a1t, so its unsure a2 is not asked; B's two unsure items reach a judge model that errors.
  const pairs: Array<[string, string, string]> = [['a1t', 'A', 'x'], ['a2', 'A', 'x'], ['b1', 'B', 'y'], ['b2', 'B', 'y']];
  const opts = optsOf(declOf(grouped('any', 'share')), pairs);
  const { api, seen } = sessionApi(clef, () => 'error');
  const k = (await round(api, opts)).checks[0];
  assert.deepEqual([k.met, k.undecided, k.undecidedItems, seen.judge.filter((r) => r.startsWith('a'))], [null, 2, undefined, []]);
  const out = await check(sessionApi(clef, () => 'error').api, opts, { kind: 'k', version: 1, hash: '', content: 'x', sealedAt: 0, verdict: null }, 'submit', BACKGROUND_CONTEXT);
  assert.deepEqual(Object.keys(out), ['undecided'], 'no verdict');
  assert.match((out as { undecided: string }).undecided, /c\.kept/);
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
  assert.deepEqual([k.met, k.notes, k.resolvers], [2 / 3, ['B: rows b1f: b1 never sets the header'], { clef: 1, j: 2 }], "c1f's flag was overturned by the judge model");
});

test('reuse inside a group: an unchanged item is reused, a changed evidence or group claim asks again', async () => {
  const { api, seen } = sessionApi(clef, () => said('no'));
  const pairs: Array<[string, string, string]> = [['a1f', 'A', 'x'], ['a2t', 'A', 'y'], ['b1t', 'B', 'x']];
  const v1 = await round(api, optsOf(declOf(grouped('any', 'share')), pairs), 1);
  assert.equal(v1.checks[0].met, 1);
  const v2 = await round(api, optsOf(declOf(grouped('any', 'share')), pairs), 2);
  assert.deepEqual([seen.clef.length, v2.checks[0].reused, v2.checks[0].resolvers], [3, { 'rows a1f': 1, 'rows a2t': 1, 'rows b1t': 1 }, {}]);
  const moved: Array<[string, string, string]> = [['a1f', 'A', 'x'], ['a2t', 'A', 'z'], ['b1t', 'C', 'x']];
  const v3 = await round(api, optsOf(declOf(grouped('any', 'share')), moved), 3);
  assert.deepEqual([seen.clef.slice(3).sort(), v3.checks[0].reused], [['a2t', 'b1t'], { 'rows a1f': 1 }], "a2t's evidence and b1t's group claim changed");
});

// Pass-only any groups: a decision-model "no" is never final, and one pass decides the group, so escalations wait for the group's answers.
const passOnlyAny = (pairs: Array<[string, string, string?]>) => optsOf(declOf(grouped('any', 'share'), { decisionModel: PASS_ONLY }), pairs);

test("pass-only any: a group whose last decision-model answer is its one pass sends nothing to the judge model", async () => {
  const { api, seen } = sessionApi(clef, () => said('no'));
  const v = await round(api, passOnlyAny([['a1f', 'A'], ['a2f', 'A'], ['a3f', 'A'], ['a4t', 'A']]));
  assert.deepEqual([v.checks[0].met, seen.clef, seen.judge], [1, ['a1f', 'a2f', 'a3f', 'a4t'], []]);
});

test('pass-only any: a group the decision model passed nowhere escalates once its answers are in, and stops at the first judge-model pass', async () => {
  const order: string[] = [];
  const { api, seen } = sessionApi((row) => (order.push(`clef ${row}`), clef(row)), (row) => (order.push(`judge ${row}`), said(row === 'a2f' ? 'yes' : 'no')));
  const v = await round(api, passOnlyAny([['a1f', 'A'], ['a2f', 'A'], ['a3f', 'A'], ['a4f', 'A']]));
  assert.ok(order.indexOf('judge a1f') > order.lastIndexOf('clef a4f'), `no escalation before the group's last answer: ${order.join(', ')}`);
  // The limiter hands a3f its slot before a2f's pass is counted, as for any check; a4f, still queued, is skipped.
  assert.deepEqual([v.checks[0].met, seen.judge.slice(0, 2), seen.judge.includes('a4f')], [1, ['a1f', 'a2f'], false]);
});

test('pass-only any: two groups interleaved across packed requests escalate only the group no part passed', async () => {
  // Each part x, y, z is one request with a question per guarantee; A passes only on the last part, B on none.
  const { api, seen } = sessionApi(clef, () => said('no'));
  const pairs: Array<[string, string, string]> = [['ax1f', 'A', 'x'], ['bx1f', 'B', 'x'], ['ay2f', 'A', 'y'], ['by2f', 'B', 'y'], ['az3t', 'A', 'z'], ['bz3f', 'B', 'z']];
  const v = await round(api, passOnlyAny(pairs));
  assert.deepEqual(seen.clef, ['ax1f', 'bx1f', 'ay2f', 'by2f', 'az3t', 'bz3f']);
  assert.deepEqual([v.checks[0].met, v.checks[0].marks, seen.judge.sort()], [0.5, ['B'], ['bx1f', 'by2f', 'bz3f']]);
});

test("pass-only any: a decision-model error on one item still releases its group's escalations, and undecided handling is unchanged", async () => {
  const { api, seen } = sessionApi((row) => (row === 'a1' ? 'error' : clef(row)), (row) => (row === 'a1' ? 'error' : said('no')));
  const v = await round(api, passOnlyAny([['a1', 'A'], ['a2f', 'A']]));
  assert.deepEqual([v.checks[0].met, v.checks[0].undecided, seen.judge.filter((r) => r === 'a2f')], [null, 1, ['a2f']]);
});

test('pass-only any: reused answers count for the group, and only its fresh items are waited on', async () => {
  const { api, seen } = sessionApi(clef, () => said('no'));
  await round(api, passOnlyAny([['a1f', 'A'], ['a2f', 'A']]), 1);
  assert.deepEqual(seen.judge.splice(0).sort(), ['a1f', 'a2f']);
  const v2 = await round(api, passOnlyAny([['a1f', 'A'], ['a2f', 'A'], ['a4f', 'A'], ['a3t', 'A'], ['b1f', 'B']]), 2);
  assert.deepEqual([v2.checks[0].met, v2.checks[0].reused, seen.clef.slice(2), seen.judge], [0.5, { 'rows a1f': 1, 'rows a2f': 1 }, ['a4f', 'a3t', 'b1f'], ['b1f']]);
});

test('a round whose items all escalate puts one judge-model call in flight on the session slot at a time', async () => {
  const flight = { now: 0, max: 0 };
  const off = gateGenerations({
    before: async (provider, _signal, shared) => {
      if (provider !== 'judge' || !shared) return;
      flight.max = Math.max(flight.max, ++flight.now);
      await new Promise((r) => setImmediate(r));
      return provider;
    },
    after: (held) => void (held && flight.now--),
  });
  const { api, seen } = sessionApi(clef, () => said('no'));
  await round(api, optsOf(declOf(grouped('any', 'share')), [['a1', 'A'], ['b1', 'B'], ['c1', 'C'], ['d1', 'D']]));
  off();
  assert.deepEqual([seen.judge.length, flight.max], [4, 1]);
});
