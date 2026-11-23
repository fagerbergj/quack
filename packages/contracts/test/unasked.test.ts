import assert from 'node:assert/strict';
import { test, type TestContext } from 'node:test';
import { score, type CheckResult, type Unit } from '@quack/judge';
import { contractDeclaration, contractRubric, type ContractSpec } from '../src/index.ts';
import { reuseKey } from '../../judge/src/reuse.ts';
import { scheduleSpecV1 } from './fixtures.ts';
import { evidenceOf, judged as judgedChecks } from './judging.ts';

// The schedule spec with one more operation that no choice decides and no definition uses: past runs, which a non-goal excludes.
function withHistory(): ContractSpec {
  const spec = scheduleSpecV1({ versioning: 'request', time: 'request' });
  const doc = spec.document as { paths: Record<string, unknown> };
  doc.paths['/apps/sleeper/history'] = { get: {
    operationId: 'getSleeperHistory',
    summary: 'Past runs, newest first',
    responses: { 200: { description: 'Past runs' }, 502: { description: 'The history service did not answer', content: { 'application/problem+json': { schema: { $ref: '#/components/schemas/Problem' } } } } },
  } };
  return spec;
}

const HISTORY = 'definition GET /apps/sleeper/history';
const unit = (units: Unit[], id: string) => units.find((u) => u.id === id)!;
const judged = (t: TestContext, spec: ContractSpec, pick: (q: string) => string) => judgedChecks(t, spec, ['adds_unasked', 'implements_non_goal'], pick);

const NAMED = 'named or implied';
const IMPLEMENTS = 'implement the non-goal';

test('code settles a definition a choice covers and one another definition uses, and neither reaches a model', async (t) => {
  const { check, asked } = await judged(t, withHistory(), (q) => (q.includes(NAMED) ? 'yes' : 'no'));
  const unasked = check('adds_unasked');
  // GET schedule is decided by the time-format and errors choices; Problem is used by both operations.
  assert.deepEqual([unasked.units, unasked.resolvers.code, unasked.met], [3, 2, 0]);
  for (const id of ['GET /apps/sleeper/schedule', '/components/schemas/Problem']) assert.ok(!asked.some((q) => q.includes(id)), `${id} was asked`);
  assert.ok(asked.some((q) => q.includes(HISTORY) && q.includes(NAMED)), 'the uncovered operation is asked');
});

test('an uncovered definition is asked the narrow questions with only the intent entries that relate to it', async () => {
  const ev = await evidenceOf(withHistory());
  const history = unit(ev.definition, HISTORY);
  assert.deepEqual(history.shared, { kind: 'rest', purpose: 'Show the Sleeper page the live week and when each job runs next.' });
  assert.deepEqual(history.state.consumersAndGuarantees, ['errors.problem-json: Errors say why in problem+json'], 'not the other guarantee, the consumer or the non-goals');
  // A definition a consumer needs in order to work was asked for, though no entry names it (the distributor mounts apps through inProcessFetch).
  assert.equal(contractDeclaration.checks.adds_unasked.question.instructions, 'Is this definition named or implied by the purpose, or by one of the consumers or guarantees shown with it, or does one of them need it in order to work?');
  // One pair per related non-goal: past runs relates to the history operation, editing schedules does not.
  assert.deepEqual(ev.nonGoal.filter((u) => u.id.startsWith(`nonGoal GET /apps/sleeper/history`)).map((u) => [u.id, (u.state.pair as { nonGoal: string }).nonGoal]), [['nonGoal GET /apps/sleeper/history ~ no-history', 'Past runs']]);
  assert.deepEqual(ev.nonGoal.filter((u) => u.related?.held).map((u) => u.id), ['nonGoal GET /apps/sleeper/schedule', 'nonGoal /components/schemas/Problem'], 'a code-settled definition is one held item, asked nothing');
});

test('an intent edit that touches none of a definition\'s related entries leaves its evidence and its reuse key as they were', async () => {
  const before = await evidenceOf(withHistory());
  const edited = withHistory();
  edited.intent.consumers.push('The Sleeper digest');
  edited.intent.guarantees.push({ id: 'week', text: 'The week is the live NFL week' });
  edited.intent.nonGoals.push({ id: 'no-scoring', text: 'Fantasy scoring' });
  const after = await evidenceOf(edited);
  const policy = { decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 }, judgeModel: { provider: 'judge', modelId: 'j' } };
  for (const [scope, check, id] of [['definition', 'adds_unasked', HISTORY], ['nonGoal', 'implements_non_goal', 'nonGoal GET /apps/sleeper/history ~ no-history']] as const) {
    const d = contractDeclaration.checks[check];
    const ask = { id: `matches_intent.${check}`, question: d.question, pass: d.aggregate.pass };
    assert.equal(reuseKey(ask, unit(after[scope], id), policy), reuseKey(ask, unit(before[scope], id), policy), `${id} is asked again`);
  }
  // Editing the entry it does relate to is a new question.
  edited.intent.guarantees[1] = { id: 'errors.problem-json', text: 'Errors say why in problem+json with a status' };
  const touched = await evidenceOf(edited);
  assert.notDeepEqual(unit(touched.definition, HISTORY).state, unit(before.definition, HISTORY).state);
});

const keyOf = (ev: Awaited<ReturnType<typeof evidenceOf>>, scope: 'definition' | 'nonGoal', check: 'adds_unasked' | 'implements_non_goal', id: string) => {
  const d = contractDeclaration.checks[check];
  const policy = { decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 }, judgeModel: { provider: 'judge', modelId: 'j' } };
  return reuseKey({ id: `matches_intent.${check}`, question: d.question, pass: d.aggregate.pass }, unit(ev[scope], id), policy);
};

test('the purpose is (a)\'s shared context: a purpose edit re-asks (a) and leaves the non-goal pairs as they were', async () => {
  const before = await evidenceOf(withHistory());
  const edited = withHistory();
  edited.intent.purpose = 'Show the Sleeper page the live NFL week and each job\'s next run.';
  const after = await evidenceOf(edited);
  assert.notEqual(keyOf(after, 'definition', 'adds_unasked', HISTORY), keyOf(before, 'definition', 'adds_unasked', HISTORY));
  const pair = 'nonGoal GET /apps/sleeper/history ~ no-history';
  assert.equal(keyOf(after, 'nonGoal', 'implements_non_goal', pair), keyOf(before, 'nonGoal', 'implements_non_goal', pair));
});

test('the rest of the document is no item of either check, and still counts as a user of what it names', async () => {
  const ev = await evidenceOf(withHistory());
  assert.ok(!ev.definition.some((u) => u.id.includes('rest of the document')));
  assert.ok(!ev.nonGoal.some((u) => u.id.includes('rest of the document')));
  const ts = { ...withHistory(), kind: 'typescript' as const, choices: [], document: '// Uses Helper.\nimport type { X } from "x";\nexport type Helper = string;\n' };
  const tsEv = await evidenceOf(ts);
  assert.deepEqual(tsEv.definition.map((u) => [u.id, u.related]), [['definition /Helper', { covered: 1 }]], 'the header names Helper, so it is settled');
});

test('the fold: an unnamed definition and one implementing a non-goal are each a share pitfall, over all definitions and all pairs', async (t) => {
  const flagged = (q: string) => q.includes('GET /apps/sleeper/history') && (q.includes(NAMED) ? 'no' : 'yes');
  const { check } = await judged(t, withHistory(), (q) => flagged(q) || (q.includes(NAMED) ? 'yes' : 'no'));
  const unasked = check('adds_unasked');
  const nonGoal = check('implements_non_goal');
  assert.equal(unasked.met, 1 / 3, 'one of three definitions, the two code settled counted as held');
  assert.deepEqual(unasked.notes.map((n) => n.slice(0, n.indexOf(':'))), [HISTORY]);
  assert.equal(nonGoal.met, 1 / nonGoal.units);
  assert.deepEqual(nonGoal.notes.map((n) => n.slice(0, n.indexOf(': '))), ['nonGoal GET /apps/sleeper/history ~ no-history']);
  assert.deepEqual([unasked.weight, nonGoal.weight], [-2, -1], 'the two halves share the weight adds_unasked had alone');
});

// The spec's score with meets_intent met and every gate clean, so only the two unasked checks cost anything.
const scoreWith = (...checks: CheckResult[]) => score(contractRubric, [{ criterion: 'matches_intent', id: 'meets_intent', label: '', weight: 4, met: true, units: 1, resolvers: {}, notes: [] }, ...checks]).score;
const yes = () => 'yes';

test('one definition of three implementing its one non-goal costs a third of the weight, not all of it', async (t) => {
  const { check } = await judged(t, withHistory(), yes);
  const nonGoal = check('implements_non_goal');
  // Three definitions, one pair: the two code-settled definitions are held items, so the share is 1/3, not 1/1.
  assert.deepEqual([nonGoal.units, nonGoal.resolvers.code, nonGoal.met], [3, 2, 1 / 3]);
  assert.equal(scoreWith(check('adds_unasked'), nonGoal), (4 - 1 / 3) / 4, '0.917 passes the 0.8 threshold; a share over the one pair would score 0.75 and fail');
});

test('several definitions implementing a non-goal cost in proportion to how many do', async (t) => {
  const spec = withHistory();
  (spec.document as { paths: Record<string, unknown> }).paths['/apps/sleeper/jobs/{job}'] = { put: { operationId: 'editJobSchedule', summary: 'Edit the schedules of a job', responses: { 204: { description: 'Edited' } } } };
  const pairs = (await evidenceOf(spec)).nonGoal.filter((u) => !u.related?.held);
  assert.ok(pairs.length >= 2, `pairs: ${pairs.map((u) => u.id).join(', ')}`);
  const all = (await judged(t, spec, yes)).check('implements_non_goal');
  const one = (await judged(t, spec, (q) => (q.includes(IMPLEMENTS) && !q.includes('history') ? 'no' : 'yes'))).check('implements_non_goal');
  assert.equal(all.met, pairs.length / all.units);
  assert.equal(one.met, 1 / one.units);
});

test('with no non-goal related to any definition, implements_non_goal asks nothing and costs nothing', async (t) => {
  const spec = withHistory();
  spec.intent.nonGoals = [];
  const { check, asked } = await judged(t, spec, yes);
  const nonGoal = check('implements_non_goal');
  assert.deepEqual([nonGoal.units, nonGoal.resolvers, nonGoal.met], [3, { code: 3 }, 0]);
  assert.ok(!asked.some((q) => q.includes(IMPLEMENTS)));
});

test('a definition with no letter or digit is an item of neither check', async () => {
  const spec = withHistory();
  (spec.document as { components: { schemas: Record<string, unknown> } }).components.schemas.Empty = {};
  // A non-goal that names it would otherwise pair with it by name, though (a) never sees it.
  spec.intent.nonGoals.push({ id: 'no-empty', text: 'An Empty placeholder schema' });
  const ev = await evidenceOf(spec);
  assert.ok(![...ev.definition, ...ev.nonGoal].some((u) => u.id.includes('Empty')));
});

test('a long consumer is matched sentence by sentence', async () => {
  const spec = withHistory();
  spec.intent.consumers[0] = 'The Sleeper page renders the live week exactly as the schedules report it, once per render, with no extra calls. It lists past runs, newest first, in the history pane.';
  const history = unit((await evidenceOf(spec)).definition, HISTORY);
  assert.deepEqual(history.state.consumersAndGuarantees, ['It lists past runs, newest first, in the history pane.', 'errors.problem-json: Errors say why in problem+json']);
});
