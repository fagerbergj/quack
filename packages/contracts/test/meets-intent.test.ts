import assert from 'node:assert/strict';
import { test } from 'node:test';
import type { Unit } from '@quack/judge';
import { contractDeclaration, contractRubric, type ContractSpec } from '../src/index.ts';
import { scheduleSpecV1 } from './fixtures.ts';
import { builtOf, judged } from './judging.ts';

const spec = () => scheduleSpecV1({ versioning: 'request', time: 'request' });
const OP = 'GET /apps/sleeper/schedule';
type Shown = { part: string; text: unknown };
const evidenceOf = (u: Unit) => u.state.evidence as unknown as Shown[];
const unit = (units: Unit[], id: string) => units.find((u) => u.id === id)!;

// The schedule spec with five components no guarantee is about, so the cut has parts to leave out.
function withComponents(): ContractSpec {
  const s = spec();
  const schemas = (s.document as { components: { schemas: Record<string, unknown> } }).components.schemas;
  for (const n of ['Avatar', 'Badge', 'Colour', 'Dimension', 'Emblem']) schemas[n] = { type: 'object', description: `A ${n.toLowerCase()} for a profile card`, properties: { label: { type: 'string' } } };
  s.intent.guarantees.push({ id: 'errors.shape', text: 'Every error body is a Problem with a title and a detail' });
  return s;
}

test('each guarantee is one item, its evidence the parts it names or shares terms with, not the unrelated ones', async () => {
  const { evidence } = await builtOf(withComponents());
  const shape = unit(evidence.guarantee, 'guarantee errors.shape');
  assert.equal(shape.state.guarantee, 'Every error body is a Problem with a title and a detail');
  const parts = evidenceOf(shape).map((e) => e.part);
  assert.ok(parts.includes('/components/schemas/Problem'), parts.join(', '));
  assert.ok(!parts.some((id) => /Avatar|Badge|Colour|Dimension|Emblem/.test(id)), parts.join(', '));
  assert.ok(parts.length <= 5, 'the four best parts and the root');
  assert.equal(shape.shared, undefined, 'nothing is packed: each promise is its own judge-model question');
});

test('a decided choice is evidence, the root always is (without its examples), and the most relevant part comes first', async () => {
  const s = withComponents();
  (s.document as Record<string, unknown>).examples = [{ big: 'an example makes nothing hold' }];
  s.intent.guarantees.push(
    { id: 'iso-times', text: 'Run times are ISO 8601 date-times' },
    { id: 'cards', text: 'Avatar, Badge, Colour and Dimension describe a profile card' },
    { id: 'live-week', text: 'The live NFL week and the next run of each scheduled job come back soonest first' },
  );
  const { evidence } = await builtOf(s);
  const shown = (id: string) => evidenceOf(unit(evidence.guarantee, `guarantee ${id}`));
  assert.deepEqual(shown('iso-times').find((e) => e.part === 'choice time-format')?.text, { decision: 'Run times are ISO 8601 date-times.', why: ['You decide (the suggested ISO 8601 string stands)', 'The service already returns the next run as the schedules service gives it.'] });
  const cards = shown('cards');
  assert.deepEqual(cards.slice(0, 4).map((e) => e.part).sort(), ['/components/schemas/Avatar', '/components/schemas/Badge', '/components/schemas/Colour', '/components/schemas/Dimension']);
  assert.equal(cards.at(-1)!.part, '(rest of the document)');
  assert.ok(!JSON.stringify(cards.at(-1)!.text).includes('an example makes nothing hold'));
  assert.equal(shown('live-week')[0].part, OP);
});

test('the judge model answers every promise and nothing reaches the decision model; a failing one carries its reason', async (t) => {
  const s = spec();
  const failed = await judged(t, s, ['meets_intent'], () => 'yes', { choice: 'no', reason: 'Nothing here marks the schedule read-only.' });
  const check = failed.check('meets_intent');
  assert.equal(check.met, false);
  assert.match(check.notes[0], /^guarantee (read-only|errors\.problem-json): Nothing here marks the schedule read-only\.$/);
  assert.deepEqual([failed.requests.length, check.resolvers.clef], [0, undefined]);
  const held = (await judged(t, s, ['meets_intent'], () => 'no', { choice: 'yes', reason: 'GET is the only method.' })).check('meets_intent');
  assert.deepEqual([held.met, held.units, held.resolvers], [true, 2, { j: 2 }]);
  assert.match(contractDeclaration.checks.meets_intent.question.instructions, /field, type, operation, header, status code or schema rule it defines, or a behaviour its own text states\?$/);
});

test('a grandfathered string guarantee keeps its key when another is inserted, and one over the fact limit is split by sentence', async () => {
  const keys = async (guarantees: string[]) => {
    const s = spec();
    s.intent.guarantees = guarantees;
    return (await builtOf(s)).evidence.guarantee.map((u) => u.id);
  };
  const before = await keys(['Read-only', 'Errors say why in problem+json']);
  const after = await keys(['Covers a week with no runs', 'Read-only', 'Errors say why in problem+json']);
  assert.deepEqual(after.slice(1), before);
  assert.match(before[0], /^guarantee Read-only #[0-9a-f]{6}$/);
  const long = 'The schedule lists every job the league runs this week, soonest first, with its next run time. A week with no runs is an empty list, not an error, and the week number is still given. Errors say why in problem+json.';
  assert.equal((await keys([long])).length, 3);
});

test('meets_intent is the one scored check, so a spec passes only when it holds; the unproven checks are gone', () => {
  const checks = contractRubric.criteria.flatMap((c) => c.checks);
  const cut = ['contradicts_guide', 'delivers_purpose', 'adds_unasked', 'implements_non_goal', 'comments_answered'];
  assert.deepEqual(checks.filter((k) => cut.includes(k.id)).map((k) => k.id), []);
  assert.deepEqual(Object.keys(contractDeclaration.checks), ['meets_intent']);
  assert.deepEqual(checks.filter((k) => typeof k.weight === 'number').map((k) => [k.id, k.weight]), [['meets_intent', 1]]);
  assert.deepEqual([contractRubric.version, contractRubric.threshold], [14, 1]);
});

test('with no guarantees, meets_intent has no items and counts toward neither side', async (t) => {
  const s = spec();
  s.intent.guarantees = [];
  const res = await judged(t, s, ['meets_intent'], () => 'no', { choice: 'yes', reason: 'Shown in GET.' });
  assert.deepEqual([res.check('meets_intent').met, res.check('meets_intent').units], [null, 0]);
});
