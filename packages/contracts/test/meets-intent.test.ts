import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
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

const fixture = (name: string) => JSON.parse(readFileSync(new URL(`fixtures-specs/${name}`, import.meta.url), 'utf8')) as ContractSpec;
const partsIn = (units: Unit[], prefix: string) => evidenceOf(units.find((u) => u.id.startsWith(prefix))!).map((e) => e.part);

// contracts:eda8542f sealed v28: the judge failed this sentence for not showing GET or DELETE /sessions/{key}, which it defines.
test('a promise gets every operation it names by path, past the four best', async () => {
  const parts = partsIn((await builtOf(fixture('sessions-api-v20.json'))).evidence.purpose, 'purpose One session catalog');
  for (const op of ['GET /sessions', 'GET /sessions/{key}', 'PATCH /sessions/{key}', 'DELETE /sessions/{key}', 'GET /sessions/{key}/artifacts', 'GET /artifacts']) assert.ok(parts.includes(op), `${op} in ${parts.join(', ')}`);
});

// contracts:608df011 v26 and v28: the catalog and extension store descriptions say "the loader checks the reference", but choices
// sharing a guide quote ("a typo should fail loudly") took the four places.
test('a choice ranks by its decision, not its whys, and "loader" meets "load"', async () => {
  const parts = partsIn((await builtOf(fixture('deployment-v17.json'))).evidence.guarantee, 'guarantee store-ref-checked');
  assert.ok(parts.includes('/properties/catalog') && parts.includes('/$defs/extension'), parts.join(', '));
  assert.ok(!parts.includes('choice top-level-open'), parts.join(', '));
});

// Three operations on one path and five shelves that share more terms with each guarantee than any operation does.
function shelves(guarantees: ContractSpec['intent']['guarantees'], shelf = 'Cached for every reader of the shelf'): ContractSpec {
  const s = spec();
  const op = (description: string) => ({ description, responses: { 204: { description: 'Done' } } });
  s.document = { openapi: '3.1.0', info: { title: 'Cache', version: '1' }, paths: { '/items/{id}': { get: op('One item'), put: op('Store an item'), delete: op('Drop an item') } }, components: { schemas: Object.fromEntries(['A', 'B', 'C', 'D', 'E'].map((n) => [`Shelf${n}`, { type: 'object', description: shelf }])) } };
  s.choices = [];
  s.intent.guarantees = guarantees;
  return s;
}

test('a verb before a path names that operation alone; a bare path names each; a promise naming nothing gets the best four and the root', async () => {
  const { evidence } = await builtOf(shelves([
    { id: 'verb', text: 'GET /items/{id} is cached for every reader' },
    { id: 'bare', text: '/items/{id} is cached for every reader' },
    { id: 'none', text: 'Everything is cached for every reader' },
  ]));
  const ops = (id: string) => partsIn(evidence.guarantee, `guarantee ${id}`).filter((p) => p.includes('/items'));
  assert.deepEqual(ops('verb'), ['GET /items/{id}']);
  assert.deepEqual(ops('bare').sort(), ['DELETE /items/{id}', 'GET /items/{id}', 'PUT /items/{id}']);
  assert.deepEqual(partsIn(evidence.guarantee, 'guarantee none'), ['/components/schemas/ShelfA', '/components/schemas/ShelfB', '/components/schemas/ShelfC', '/components/schemas/ShelfD', '(rest of the document)']);
});

test('over the evidence ceiling the lowest-ranked parts go, never one the promise names', async () => {
  const { evidence } = await builtOf(shelves([{ id: 'verb', text: 'GET /items/{id} is cached for every reader' }], `Cached for every reader of the shelf. ${'x'.repeat(20_000)}`));
  assert.deepEqual(partsIn(evidence.guarantee, 'guarantee verb'), ['/components/schemas/ShelfA', '/components/schemas/ShelfB', 'GET /items/{id}', '(rest of the document)']);
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

test('each promising sentence of the purpose is one judge-model item; provenance is not; the two checks share the old weight of 4', async (t) => {
  const s = spec();
  s.intent.purpose = 'Show the Sleeper page the live week and when each job runs next. It only reads. Provenance: v2 was approved after the epoch change. Provenance: Derived from issue #123.';
  const { evidence } = await builtOf(s);
  assert.deepEqual(evidence.purpose.map((u) => u.state.sentence), ['Show the Sleeper page the live week and when each job runs next.', 'It only reads.']);
  assert.ok(evidenceOf(evidence.purpose[0]).some((e) => e.part === OP));
  const failed = await judged(t, s, ['delivers_purpose'], () => 'yes', { choice: 'no', reason: 'No part reports the live week.' });
  assert.deepEqual([failed.check('delivers_purpose').met, failed.requests.length], [false, 0]);
  assert.match(failed.check('delivers_purpose').notes[0], /^purpose .+: No part reports the live week\.$/);
  const weights = contractRubric.criteria.flatMap((c) => c.checks).filter((k) => ['meets_intent', 'delivers_purpose'].includes(k.id)).map((k) => k.weight);
  assert.deepEqual(weights, [3, 1]);
});

test('no choice is judged against the guide; the scored weights are 4 possible against 3 of pitfalls, so a pitfall alone keeps its pass line', () => {
  const checks = contractRubric.criteria.flatMap((c) => c.checks);
  assert.ok(!checks.some((k) => k.id === 'contradicts_guide') && !('contradicts_guide' in contractDeclaration.checks));
  const scored = checks.filter((k) => typeof k.weight === 'number').map((k) => [k.id, k.weight]);
  assert.deepEqual(scored, [['meets_intent', 3], ['delivers_purpose', 1], ['adds_unasked', -2], ['implements_non_goal', -1]]);
  assert.deepEqual([contractRubric.version, contractRubric.threshold], [12, 0.8]);
});

test('with no guarantees, meets_intent has no items and counts toward neither side while delivers_purpose still scores', async (t) => {
  const s = spec();
  s.intent.guarantees = [];
  s.intent.purpose = 'Show the Sleeper page the live week. It only reads.';
  const res = await judged(t, s, ['meets_intent', 'delivers_purpose'], () => 'no', { choice: 'yes', reason: 'Shown in GET.' });
  assert.deepEqual([res.check('meets_intent').met, res.check('meets_intent').units], [null, 0]);
  assert.deepEqual([res.check('delivers_purpose').met, res.check('delivers_purpose').units], [true, 2]);
});
