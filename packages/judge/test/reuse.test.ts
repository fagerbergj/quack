import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { PASS_ONLY, type Declaration, type Question } from '../src/declaration.ts';
import { rejudge, report, type Inputs, type JudgeOptions, type Rubric } from '../src/index.ts';
import { PROMPT_VERSION } from '../src/resolve.ts';
import { reuseKey } from '../src/reuse.ts';
import { sessionApi } from './fake-session.ts';

const said = (choice: 'yes' | 'no', reason = `the judge says ${choice}`) => JSON.stringify({ choice, confidence: 0.9, reason });
const question: Question = { type: 'bool', instructions: 'Does it keep the rule?' };
const lone = { split: { name: 'jsonItems', from: 'rows' }, piece: 'row' };
const packed = { ...lone, context: { guide: { name: 'json', from: 'guide' } } };
const declOf = (aggregate: Declaration['checks'][string]['aggregate'], more: Partial<Declaration['checks'][string]> = {}, scope = lone): Declaration => ({ scopes: { rows: scope }, checks: { kept: { scope: 'rows', question, aggregate, ...more } } });
const rubricOf = (version = 1, extra: Rubric['criteria'][number]['checks'] = []): Rubric => ({ name: 'r', version, threshold: 0.5, criteria: [{ id: 'c', checks: [{ id: 'kept', label: 'kept', weight: 2 }, ...extra] }] });
const rowsJson = (...rows: Array<string | Record<string, unknown>>) => JSON.stringify(rows.map((r) => (typeof r === 'string' ? { id: r } : r)));
const optsOf = (declaration: Declaration, inputs: Inputs, rubric = rubricOf()): JudgeOptions => ({
  kind: 'k', rubric, declaration, inputs: async () => ({ guide: '{"rule":"r"}', ...inputs }), maxRevisions: 2, reuse: true,
  decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 }, judgeModel: { provider: 'judge', modelId: 'j' },
});
const sealedV = (version: number) => ({ kind: 'k', version, hash: '', content: 'x', sealedAt: 0, verdict: null });
const round = (api: never, opts: JudgeOptions, version: number, memo = `m${version}`) => rejudge(api, opts, sealedV(version), memo, BACKGROUND_CONTEXT);

test('an identical item on the next version is not asked: it is reused with the version first judged on, and resolvers count this round only', async () => {
  const { api, seen } = sessionApi(() => 0.95, () => said('yes'));
  const opts = optsOf(declOf({ name: 'share' }), { rows: rowsJson('a', 'b', 'c') });
  const v1 = await round(api, opts, 1);
  assert.deepEqual([v1.checks[0].resolvers, v1.checks[0].reused], [{ clef: 3 }, undefined]);
  const v2 = await round(api, opts, 2);
  assert.deepEqual(seen.clef, ['a', 'b', 'c'], 'v2 asked nothing');
  assert.deepEqual([v2.checks[0].met, v2.checks[0].resolvers, v2.checks[0].reused], [1, {}, { 'rows a': 1, 'rows b': 1, 'rows c': 1 }]);
  const v3 = await round(api, optsOf(declOf({ name: 'share' }), { rows: rowsJson('a', { id: 'b', n: 1 }, 'c') }), 3);
  assert.deepEqual(seen.clef.slice(3), ['b']);
  const k = v3.checks[0];
  assert.deepEqual([k.resolvers, k.reused], [{ clef: 1 }, { 'rows a': 1, 'rows c': 1 }]);
  assert.equal(Object.values(k.resolvers).reduce((a, b) => a + b, 0) + Object.keys(k.reused!).length, k.units, 'resolvers plus reused cover the settled items');
});

test('a changed evidence byte or a changed question asks again; a changed rubric version does not', async () => {
  const { api, seen } = sessionApi(() => 0.95, () => said('yes'));
  const inputs = { rows: rowsJson('a', { id: 'b', n: 1 }) };
  await round(api, optsOf(declOf({ name: 'share' }), inputs), 1);
  await round(api, optsOf(declOf({ name: 'share' }), { rows: rowsJson('a', { id: 'b', n: 2 }) }), 2);
  assert.deepEqual(seen.clef.splice(0), ['a', 'b', 'b'], 'one changed byte in b');
  await round(api, optsOf(declOf({ name: 'share' }), inputs), 3);
  await round(api, optsOf({ ...declOf({ name: 'share' }), checks: { kept: { scope: 'rows', question: { ...question, instructions: 'Does it keep the rules?' }, aggregate: { name: 'share' } } } }, inputs), 4);
  await round(api, optsOf(declOf({ name: 'share', pass: ['no'] }), inputs), 5);
  assert.deepEqual(seen.clef.splice(0), ['a', 'b', 'a', 'b'], 'a changed instruction, then a changed pass, asks every item');
  const v6 = await round(api, optsOf(declOf({ name: 'share' }), inputs, rubricOf(2)), 6);
  assert.deepEqual([seen.clef, v6.rubric, v6.checks[0].reused], [[], 'r@2', { 'rows a': 1, 'rows b': 1 }]);
});

test('a failure is reused and its note says which version it was judged on', async () => {
  const { api, seen } = sessionApi((row) => (row === 'b' ? 0.05 : 0.95), () => said('yes'));
  const opts = optsOf(declOf({ name: 'share' }), { rows: rowsJson('a', 'b', 'c') });
  const v1 = await round(api, opts, 1);
  assert.deepEqual(v1.checks[0].notes, ['rows b: flagged by clef (no reason given)']);
  const v2 = await round(api, opts, 2);
  assert.equal(seen.clef.length, 3);
  assert.deepEqual([v2.checks[0].met, v2.checks[0].notes], [2 / 3, ['rows b: flagged by clef (no reason given) (judged on v1)']]);
});

test('an undecided item and an outage are not stored and are asked next round', async () => {
  let answering = false;
  // a: Clef unsure and the judge model's reply unparsed; b: both models down; c: Clef sure.
  const { api, store, seen } = sessionApi((row) => (row === 'b' && !answering ? 'error' : row === 'a' ? 0.5 : 0.95), (row) => (answering ? said('yes') : row === 'a' ? 'no idea' : 'error'));
  const opts = optsOf(declOf({ name: 'share' }), { rows: rowsJson('a', 'b', 'c') });
  const v1 = await round(api, opts, 1);
  assert.deepEqual([v1.checks[0].undecided, Object.keys(store.answers).length], [2, 1], 'only c is kept');
  answering = true;
  const v2 = await round(api, opts, 2);
  assert.deepEqual([seen.clef.slice(3), seen.judge.slice(-1)], [['a', 'b'], ['a']]);
  assert.deepEqual([v2.checks[0].met, v2.checks[0].reused, v2.checks[0].resolvers], [1, { 'rows c': 1 }, { clef: 1, j: 1 }]);
});

test('an item over the decision model cap that no model answered is not stored, and is escalated again next round', async () => {
  let answering = false;
  const { api, store, seen } = sessionApi(() => 0.95, () => (answering ? said('yes') : 'error'));
  const opts = { ...optsOf(declOf({ name: 'share' }), { rows: rowsJson('a') }), decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 1 } };
  const v1 = await round(api, opts, 1);
  assert.deepEqual([v1.checks[0].undecided, store.answers, seen.clef], [1, {}, []]);
  answering = true;
  const v2 = await round(api, opts, 2);
  assert.deepEqual([seen.judge, v2.checks[0].resolvers, Object.keys(store.answers).length], [['a', 'a'], { j: 1 }, 1]);
});

test("under pass-only the decision model's failing answer is not stored; the judge model's answer to it is", async () => {
  let reply = 'no idea';
  // The check holds on "yes"; Clef passes a and confidently fails b.
  const { api, store, seen } = sessionApi((row) => (row === 'b' ? 0.05 : 0.95), () => reply);
  const opts = optsOf(declOf({ name: 'share' }, { decisionModel: PASS_ONLY }), { rows: rowsJson('a', 'b') });
  const v1 = await round(api, opts, 1);
  assert.deepEqual([v1.checks[0].undecided, Object.values(store.answers).map((e) => e.resolver)], [1, ['clef']], "Clef's fail on b went to the judge model, which gave nothing");
  reply = said('no', 'b states two counts');
  await round(api, opts, 2);
  assert.deepEqual(Object.values(store.answers).map((e) => [e.resolver, e.met, e.version]), [['clef', true, 1], ['j', false, 2]]);
  const v3 = await round(api, opts, 3);
  assert.deepEqual([seen.clef, seen.judge], [['a', 'b', 'b'], ['b', 'b']], 'v3 asked neither model');
  assert.deepEqual(v3.checks[0].notes, ['rows b: b states two counts (judged on v2)']);
});

test('code checks rerun every round and are never reused', async () => {
  let runs = 0;
  const { api } = sessionApi(() => 0.95, () => said('yes'));
  const code = { id: 'short', label: 'short', weight: 1, code: () => (runs++, { met: 1 }) };
  const opts = optsOf(declOf({ name: 'share' }), { rows: rowsJson('a') }, rubricOf(1, [code]));
  await round(api, opts, 1);
  const v2 = await round(api, opts, 2);
  const k = v2.checks.find((c) => c.id === 'short')!;
  assert.deepEqual([runs, k.resolvers, k.reused], [2, { code: 1 }, undefined]);
  assert.deepEqual(v2.checks.find((c) => c.id === 'kept')!.reused, { 'rows a': 1 }, 'beside the model check, whose item was reused');
});

test('the fold is recomputed from reused answers: a check they already decide escalates nothing more, as with fresh answers', async () => {
  // Clef fails b outright and is unsure of d, which only the next version has.
  const { api, store, seen } = sessionApi((row) => (row === 'b' ? 0.05 : row === 'd' ? 0.5 : 0.95), () => said('yes'));
  await round(api, optsOf(declOf({ name: 'all' }), { rows: rowsJson('a', 'b', 'c') }), 1);
  const v2 = await round(api, optsOf(declOf({ name: 'all' }), { rows: rowsJson('a', 'b', 'd') }), 2);
  assert.deepEqual([seen.clef, seen.judge], [['a', 'b', 'c'], []], 'the reused failure of b decides the all fold, so d asks neither model');
  const k = v2.checks[0];
  assert.deepEqual([k.met, k.notes, k.undecidedItems, k.reused, k.resolvers], [false, ['rows b: flagged by clef (no reason given) (judged on v1)'], ['rows d'], { 'rows a': 1, 'rows b': 1 }, {}]);
  assert.equal(Object.keys(store.answers).length, 3, 'd, skipped, is not stored');
});

test('a resubmit of the same version after an undecided round asks only the items it left undecided', async () => {
  let resubmitted = false;
  // Every row is unsure for Clef; the packed judge-model reply leaves d out, and d's own escalation is unparsed the first time.
  const { api, seen } = sessionApi((row) => (row === 'd' && resubmitted ? 0.95 : 0.5), (row) => (row !== 'd' ? said('yes') : resubmitted ? said('yes') : 'no idea'));
  const opts = optsOf(declOf({ name: 'share' }, {}, packed), { rows: rowsJson('a', 'b', 'c', 'd') });
  const first = await round(api, opts, 1, 'judge:k:v1 call 1');
  assert.deepEqual(first.undecided, ['c.kept']);
  resubmitted = true;
  const again = await round(api, opts, 1, 'judge:k:v1 call 2');
  assert.deepEqual(seen.clef.slice(4), ['d'], 'only d is asked again');
  assert.deepEqual([again.checks[0].met, again.checks[0].reused, again.checks[0].resolvers], [1, { 'rows a': 1, 'rows b': 1, 'rows c': 1 }, { clef: 1 }]);
});

test('rejudge with reuse absent asks every item though the store holds it, and writes nothing to the store', async () => {
  const { api, store, seen } = sessionApi(() => 0.95, () => said('yes'));
  const opts = optsOf(declOf({ name: 'share' }), { rows: rowsJson('a', 'b') });
  const { reuse: _, ...fresh } = opts;
  await round(api, opts, 1);
  const kept = structuredClone(store.answers);
  const commits = seen.commits;
  const v2 = await round(api, fresh, 2);
  assert.deepEqual([seen.clef, seen.commits, store.answers, v2.checks[0].reused, v2.checks[0].resolvers], [['a', 'b', 'a', 'b'], commits, kept, undefined, { clef: 2 }]);
  assert.equal(Object.keys(kept).length, 2);
});

test('a prompt version bump asks every stored item again', async () => {
  const { api, store, seen } = sessionApi(() => 0.95, () => said('yes'));
  const opts = optsOf(declOf({ name: 'share' }), { rows: rowsJson('a', 'b') });
  await round(api, opts, 1);
  const ask = { id: 'c.kept', question, pass: undefined };
  const unit = (id: string) => ({ id: `rows ${id}`, state: { row: { id } } });
  const keyed = (v: number) => ['a', 'b'].map((id) => reuseKey(ask, unit(id), { decisionModel: opts.decisionModel, judgeModel: opts.judgeModel }, v));
  assert.deepEqual(Object.keys(store.answers).sort(), keyed(PROMPT_VERSION).sort(), 'the store is keyed under the current prompt version');
  // The same answers as a judge with the next prompt version would look them up.
  const bumped = keyed(PROMPT_VERSION + 1);
  store.answers = Object.fromEntries(Object.values(store.answers).map((e, i) => [bumped[i], e]));
  await round(api, opts, 2);
  assert.deepEqual(seen.clef, ['a', 'b', 'a', 'b']);
});

test('a reused answer is not an escalation: only the items asked this round are counted in escalated', async () => {
  const { api } = sessionApi((row) => (row === 'a' || row === 'd' ? 0.5 : 0.95), () => said('yes'));
  const v1 = await round(api, optsOf(declOf({ name: 'share' }), { rows: rowsJson('a', 'b') }), 1);
  assert.deepEqual([v1.checks[0].escalated, v1.checks[0].reused], [{ unsure: 1 }, undefined]);
  const v2 = await round(api, optsOf(declOf({ name: 'share' }), { rows: rowsJson('a', 'b', 'd') }), 2);
  assert.deepEqual([v2.checks[0].escalated, v2.checks[0].reused], [{ unsure: 1 }, { 'rows a': 1, 'rows b': 1 }], 'a, escalated on v1, is reused and not counted again');
});

test('a failed code gate returns at once: no model is asked, the model checks are listed as not run and left out of the score, and the next version reuses earlier answers', async () => {
  const { api, seen } = sessionApi(() => 0.95, () => said('yes'));
  let content = 'ok';
  const gate = { id: 'says_ok', label: 'Does not say ok', weight: 'gate' as const, code: () => ({ met: content === 'ok' ? 1 : 0 }) };
  const opts = optsOf(declOf({ name: 'share' }), { rows: rowsJson('a', 'b') }, rubricOf(1, [gate]));
  const v1 = await round(api, opts, 1);
  assert.deepEqual([v1.passed, seen.clef], [true, ['a', 'b']], 'gates pass, so the model checks run');
  content = 'no';
  const v2 = await round(api, opts, 2);
  assert.deepEqual([v2.passed, v2.gatesFailed, v2.undecided, seen.clef.length, seen.judge], [false, ['c.says_ok'], [], 2, []]);
  assert.deepEqual(v2.checks.map((k) => [k.id, k.met, k.deferred]), [['says_ok', 0, undefined], ['kept', null, 'gate']]);
  assert.equal(v2.score, 1, 'the unrun model check adds nothing to the score; the gate fails the version');
  assert.match(report(sealedV(2), v2, 'submit'), /c\.kept not run because a gate failed, they run once the gates pass\), failed:\n- gate c\.says_ok/);
  content = 'ok';
  const v3 = await round(api, opts, 3);
  assert.deepEqual([v3.passed, seen.clef.length, v3.checks.find((k) => k.id === 'kept')!.reused], [true, 2, { 'rows a': 1, 'rows b': 1 }]);
});
