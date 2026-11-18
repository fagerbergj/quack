import assert from 'node:assert/strict';
import { test } from 'node:test';
import { contractDeclaration, type ContractSpec } from '../src/index.ts';
import { pack } from '../../judge/src/resolve.ts';
import { scheduleSpecV1 } from './fixtures.ts';
import { builtOf, judged } from './judging.ts';

const MECHANISM = 'make this guarantee hold';
const spec = () => scheduleSpecV1({ versioning: 'request', time: 'request' });
const OP = 'GET /apps/sleeper/schedule';

// The schedule spec with five components no guarantee is about, so the cut has parts to leave out.
function withComponents(): ContractSpec {
  const s = spec();
  const schemas = (s.document as { components: { schemas: Record<string, unknown> } }).components.schemas;
  for (const n of ['Avatar', 'Badge', 'Colour', 'Dimension', 'Emblem']) schemas[n] = { type: 'object', description: `A ${n.toLowerCase()} for a profile card`, properties: { label: { type: 'string' } } };
  s.intent.guarantees.push({ id: 'errors.shape', text: 'Every error body is a Problem with a title and a detail' });
  return s;
}

test('meets_intent asks each guarantee of the parts related to it, grouped by guarantee, with the part as shared context', async () => {
  const { evidence, groups } = await builtOf(withComponents());
  const units = evidence.guarantee;
  // The guarantee that names Problem is asked of the Problem part and not of the five unrelated components.
  const shape = units.filter((u) => groups!.guarantee[u.id] === 'errors.shape').map((u) => (u.shared!.part as { id: string }).id);
  assert.ok(shape.includes('/components/schemas/Problem'), shape.join(', '));
  assert.ok(!shape.some((id) => /Avatar|Badge|Colour|Dimension|Emblem/.test(id)), shape.join(', '));
  assert.ok(shape.length <= 4);
  const one = units.find((u) => u.id === `guarantee errors.problem-json ~ ${OP}`)!;
  assert.deepEqual(Object.keys(one.shared!), ['part', 'kind']);
  assert.deepEqual(one.state, { guarantee: 'Errors say why in problem+json' });
  // Every guarantee asked of one part shares that part, so the part goes once per request.
  const ask = { id: 'matches_intent.meets_intent', question: contractDeclaration.checks.meets_intent.question, pass: ['yes'] };
  const requests = pack(units.map((unit) => ({ unit, checks: [ask] })), 8192);
  assert.equal(requests.length, new Set(units.map((u) => (u.shared!.part as { id: string }).id)).size);
});

test('a behaviour the part states in its own text counts as making the guarantee hold, not only a field, type or schema rule', () => {
  // Contracts written as TypeScript or JSON Schema state behaviour in TSDoc and descriptions; that text is the contract.
  assert.match(contractDeclaration.checks.meets_intent.question.instructions, /field, type, operation, header, status code or schema rule it defines, or a behaviour its own text states\?$/);
});

test('a guarantee holds when one part gives it a mechanism, and the check fails, naming the guarantee and part, when one has none', async (t) => {
  const held = await judged(t, spec(), ['meets_intent'], (q) => (q.includes(MECHANISM) && q.includes(OP) ? 'yes' : 'no'));
  const ok = held.check('meets_intent');
  assert.deepEqual([ok.met, ok.groups, ok.resolvers.clef > 0], [true, 2, true]);
  const no = { choice: 'no', reason: 'Nothing in this part marks the schedule read-only.' };
  const missing = await judged(t, spec(), ['meets_intent'], (q) => (q.includes(MECHANISM) && q.includes(OP) && !q.includes('read-only') ? 'yes' : 'no'), no);
  const failed = missing.check('meets_intent');
  assert.equal(failed.met, false);
  assert.equal(failed.notes.length, 1);
  assert.match(failed.notes[0], /^read-only: guarantee read-only ~ .+: Nothing in this part marks the schedule read-only\.$/);
});

test('a Clef "no" is not final: the judge model decides it, so a failing guarantee has a reason, and its "yes" holds the guarantee', async (t) => {
  const s = spec();
  s.intent.guarantees = [{ id: 'read-only', text: 'Read-only' }];
  const flagged = await judged(t, s, ['meets_intent'], () => 'no', { choice: 'no', reason: 'No part says the schedule cannot be changed.' });
  const failed = flagged.check('meets_intent');
  assert.equal(failed.met, false);
  assert.equal(failed.resolvers.clef, undefined, 'no item is failed on the decision model alone');
  assert.ok(failed.resolvers.j > 0);
  assert.match(failed.notes[0], /No part says the schedule cannot be changed\.$/);
  const overturned = (await judged(t, s, ['meets_intent'], () => 'no', { choice: 'yes', reason: 'GET is the only method, so the schedule is read-only.' })).check('meets_intent');
  assert.equal(overturned.met, true);
});

test('a decided choice is a part a guarantee can be asked of, and the root is asked even when four named parts outrank it', async () => {
  const s = withComponents();
  s.intent.guarantees.push({ id: 'iso-times', text: 'Run times are ISO 8601 date-times' }, { id: 'cards', text: 'Avatar, Badge, Colour and Dimension describe a profile card' });
  const { evidence, groups } = await builtOf(s);
  const partsOf = (key: string) => evidence.guarantee.filter((u) => groups!.guarantee[u.id] === key).map((u) => u.shared!.part as { id: string; definition: unknown });
  const iso = partsOf('iso-times').find((p) => p.id === 'choice time-format');
  assert.deepEqual(iso?.definition, { decision: 'Run times are ISO 8601 date-times.', why: ['You decide (the suggested ISO 8601 string stands)', 'The service already returns the next run as the schedules service gives it.'] });
  const cards = partsOf('cards').map((p) => p.id);
  assert.deepEqual(cards.slice(0, 4).sort(), ['/components/schemas/Avatar', '/components/schemas/Badge', '/components/schemas/Colour', '/components/schemas/Dimension']);
  assert.equal(cards.at(-1), '(rest of the document)');
});

test('a guarantee is asked of its parts most relevant first, so a failing note names the part sharing the most terms with it', async (t) => {
  const s = spec();
  // "week" is a component's name and a plain word of the guarantee; the operation shares far more of the guarantee's terms.
  (s.document as { components: { schemas: Record<string, unknown> } }).components.schemas.week = { type: 'integer', minimum: 1 };
  s.intent.guarantees = [{ id: 'live-week', text: 'The live NFL week and the next run of each scheduled job come back soonest first' }];
  const { evidence } = await builtOf(s);
  const parts = evidence.guarantee.map((u) => (u.shared!.part as { id: string }).id);
  assert.ok(parts.includes('/components/schemas/week'), 'the named part is still asked');
  assert.equal(parts[0], OP);
  const failed = (await judged(t, s, ['meets_intent'], () => 'no', { choice: 'no', reason: 'Not here.' })).check('meets_intent');
  assert.equal(failed.notes[0], `live-week: guarantee live-week ~ ${OP}: Not here.`);
});

test('a grandfathered string guarantee keeps its key when another is inserted, and one over the fact limit is split by sentence', async () => {
  const keys = async (guarantees: string[]) => {
    const s = spec();
    s.intent.guarantees = guarantees;
    return [...new Set(Object.values((await builtOf(s)).groups!.guarantee))];
  };
  const before = await keys(['Read-only', 'Errors say why in problem+json']);
  const after = await keys(['Covers a week with no runs', 'Read-only', 'Errors say why in problem+json']);
  assert.deepEqual(after.slice(1), before);
  assert.match(before[0], /^Read-only #[0-9a-f]{6}$/);
  const long = 'The schedule lists every job the league runs this week, soonest first, with its next run time. A week with no runs is an empty list, not an error, and the week number is still given. Errors say why in problem+json.';
  assert.equal((await keys([long])).length, 3);
});

test('with no guarantees, the purpose stands in as the one guarantee', async () => {
  const s = spec();
  s.intent.guarantees = [];
  const { evidence, groups } = await builtOf(s);
  assert.deepEqual([...new Set(Object.values(groups!.guarantee))], ['purpose']);
  assert.ok(evidence.guarantee.every((u) => u.state.guarantee === s.intent.purpose));
});
