import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { decisionModelTokens, PASS_ONLY, type Unit } from '@quack/judge';
import { contractDeclaration, contractRubric, type ContractSpec } from '../src/index.ts';
import { scheduleSpecV1 } from './fixtures.ts';
import { builtOf, judged } from './judging.ts';

const spec = () => scheduleSpecV1({ versioning: 'request', time: 'request' });
const OP = 'GET /apps/sleeper/schedule';
const ROOT = '(rest of the document)';
const fixture = (name: string) => readFileSync(new URL(`fixtures-specs/${name}`, import.meta.url), 'utf8');
const specFixture = (name: string) => JSON.parse(fixture(name)) as ContractSpec;
// The parts a guarantee is paired with, by the unit ids "guarantee <key> ~ <part>".
const partsOf = (units: Unit[], key: string) => units.filter((u) => u.id.startsWith(`guarantee ${key} ~ `)).map((u) => u.id.slice(`guarantee ${key} ~ `.length));
const pairsOf = async (s: ContractSpec, key: string) => partsOf((await builtOf(s)).evidence.guarantee, key);

// The schedule spec with five components no guarantee is about, so the cut has parts to leave out.
function withComponents(): ContractSpec {
  const s = spec();
  const schemas = (s.document as { components: { schemas: Record<string, unknown> } }).components.schemas;
  for (const n of ['Avatar', 'Badge', 'Colour', 'Dimension', 'Emblem']) schemas[n] = { type: 'object', description: `A ${n.toLowerCase()} for a profile card`, properties: { label: { type: 'string' } } };
  s.intent.guarantees.push({ id: 'errors.shape', text: 'Every error body is a Problem with a title and a detail' });
  return s;
}

test('each item is one guarantee and one part; the guarantee is the shared context, so its pairs pack', async () => {
  const { evidence, groups } = await builtOf(withComponents());
  const shape = evidence.guarantee.filter((u) => u.id.startsWith('guarantee errors.shape ~ '));
  const parts = shape.map((u) => u.id.split(' ~ ')[1]);
  assert.ok(parts.includes('/components/schemas/Problem'), parts.join(', '));
  assert.ok(!parts.some((id) => /Avatar|Badge|Colour|Dimension|Emblem/.test(id)), parts.join(', '));
  assert.ok(parts.length <= 5, 'the four best parts and the root');
  assert.deepEqual(shape[0].shared, { guarantee: 'Every error body is a Problem with a title and a detail' });
  assert.deepEqual(Object.keys(shape[0].state), ['part']);
  assert.deepEqual(new Set(shape.map((u) => groups!.guarantee[u.id])), new Set(['errors.shape']));
});

test('a decided choice is a part, the root is paired with every guarantee (without its examples), and a named part always is', async () => {
  const s = withComponents();
  (s.document as Record<string, unknown>).examples = [{ big: 'an example makes nothing hold' }];
  s.intent.guarantees.push(
    { id: 'iso-times', text: 'Run times are ISO 8601 date-times' },
    { id: 'cards', text: 'Avatar, Badge, Colour, Dimension and Emblem describe a profile card' },
    { id: 'live-week', text: 'The live NFL week and the next run of each scheduled job come back soonest first' },
  );
  const { evidence } = await builtOf(s);
  const unit = (id: string) => evidence.guarantee.find((u) => u.id === `guarantee ${id}`)!;
  assert.deepEqual(unit('iso-times ~ choice time-format').state.part, { decision: 'Run times are ISO 8601 date-times.', why: ['You decide (the suggested ISO 8601 string stands)', 'The service already returns the next run as the schedules service gives it.'] });
  const cards = partsOf(evidence.guarantee, 'cards');
  for (const n of ['Avatar', 'Badge', 'Colour', 'Dimension', 'Emblem']) assert.ok(cards.includes(`/components/schemas/${n}`), `${n}: five named parts, past the four best`);
  assert.ok(cards.includes(ROOT) && !JSON.stringify(unit(`cards ~ ${ROOT}`).state).includes('an example makes nothing hold'));
  assert.ok(partsOf(evidence.guarantee, 'live-week').includes(OP));
});

// contracts:608df011 v26 and v28: the catalog and extension store descriptions say "the loader checks the reference", but choices
// sharing a guide quote ("a typo should fail loudly") took the four places, and the judge failed a guarantee the document defines.
test('a choice ranks by its decision, not its whys, and "loader" meets "load"', async () => {
  const parts = await pairsOf(specFixture('deployment-v17.json'), 'store-ref-checked');
  assert.ok(parts.includes('/properties/catalog') && parts.includes('/$defs/extension'), parts.join(', '));
  assert.ok(!parts.includes('choice top-level-open'), parts.join(', '));
});

// contracts:eda8542f sealed v28: the judge failed a promise for not showing GET or DELETE /sessions/{key}, which the document defines.
test('a guarantee is paired with every operation it names by path', async () => {
  const s = specFixture('sessions-api-v20.json');
  s.intent.guarantees = [{ id: 'catalog', text: 'Lists come from GET /sessions and GET /artifacts; one row is GET /sessions/{key}, renamed by PATCH /sessions/{key} and removed by DELETE /sessions/{key}' }];
  const parts = await pairsOf(s, 'catalog');
  for (const op of ['GET /sessions', 'GET /sessions/{key}', 'PATCH /sessions/{key}', 'DELETE /sessions/{key}', 'GET /artifacts']) assert.ok(parts.includes(op), `${op} in ${parts.join(', ')}`);
});

// Three operations on one path and five shelves that share more terms with each guarantee than any operation does.
function shelves(guarantees: ContractSpec['intent']['guarantees']): ContractSpec {
  const s = spec();
  const op = (description: string) => ({ description, responses: { 204: { description: 'Done' } } });
  s.document = { openapi: '3.1.0', info: { title: 'Cache', version: '1' }, paths: { '/items/{id}': { get: op('One item'), put: op('Store an item'), delete: op('Drop an item') } }, components: { schemas: Object.fromEntries(['A', 'B', 'C', 'D', 'E'].map((n) => [`Shelf${n}`, { type: 'object', description: 'Cached for every reader of the shelf' }])) } };
  s.choices = [];
  s.intent.guarantees = guarantees;
  return s;
}

test('a verb before a path names that operation alone; a bare path names each; a guarantee naming nothing gets the best four and the root', async () => {
  const { evidence } = await builtOf(shelves([
    { id: 'verb', text: 'GET /items/{id} is cached for every reader' },
    { id: 'bare', text: '/items/{id} is cached for every reader' },
    { id: 'none', text: 'Everything is cached for every reader' },
  ]));
  const ops = (id: string) => partsOf(evidence.guarantee, id).filter((p) => p.includes('/items'));
  assert.deepEqual(ops('verb'), ['GET /items/{id}']);
  assert.deepEqual(ops('bare').sort(), ['DELETE /items/{id}', 'GET /items/{id}', 'PUT /items/{id}']);
  assert.deepEqual(partsOf(evidence.guarantee, 'none'), ['/components/schemas/ShelfA', '/components/schemas/ShelfB', '/components/schemas/ShelfC', '/components/schemas/ShelfD', ROOT]);
});

// Clef answers per pair: `holds` names the parts it says define the guarantee.
const holdsIn = (...parts: string[]) => (q: string) => (parts.some((p) => q.includes(`~ ${p})`)) ? 'yes' : 'no');

test('one part holding passes its guarantee and the rest of its pairs are not escalated; Clef settles it all, with no judge model', async (t) => {
  const s = spec();
  const res = await judged(t, s, ['meets_intent'], holdsIn(ROOT));
  const check = res.check('meets_intent');
  assert.deepEqual([check.met, check.groups, check.resolvers.j], [true, 2, undefined]);
  assert.ok(res.requests.length > 0 && res.requests.length <= 2, 'one packed decision-model request per guarantee');
});

test("Clef's no fails nothing: the guarantee's pairs go to the judge model, and its no and reason are the note", async (t) => {
  const s = spec();
  const pick = (q: string) => (q.includes('(guarantee read-only ~') ? 'no' : holdsIn(ROOT)(q));
  const failed = (await judged(t, s, ['meets_intent'], pick, { choice: 'no', reason: 'Nothing here marks the schedule read-only.' })).check('meets_intent');
  const pairs = (await builtOf(s)).evidence.guarantee.filter((u) => u.id.startsWith('guarantee read-only ~ ')).length;
  assert.deepEqual([failed.met, failed.marks, failed.resolvers.j], [false, ['read-only'], pairs], 'every pair of the failing guarantee, and none of the one Clef passed');
  assert.match(failed.notes[0], /^guarantee read-only ~ .+: Nothing here marks the schedule read-only\.$/);
  const held = (await judged(t, s, ['meets_intent'], pick, { choice: 'yes', reason: 'GET is the only method.' })).check('meets_intent');
  assert.deepEqual([held.met, held.resolvers.j > 0], [true, true], "the judge model's yes on a pair Clef said no to holds the guarantee");
});

test('a grandfathered string guarantee keeps its key when another is inserted, and one over the fact limit is split by sentence', async () => {
  const keys = async (guarantees: string[]) => {
    const s = spec();
    s.intent.guarantees = guarantees;
    const { evidence, groups } = await builtOf(s);
    return [...new Set(evidence.guarantee.map((u) => groups!.guarantee[u.id]))];
  };
  const before = await keys(['Read-only', 'Errors say why in problem+json']);
  const after = await keys(['Covers a week with no runs', 'Read-only', 'Errors say why in problem+json']);
  assert.deepEqual(after.slice(1), before);
  assert.match(before[0], /^Read-only #[0-9a-f]{6}$/);
  const long = 'The schedule lists every job the league runs this week, soonest first, with its next run time. A week with no runs is an empty list, not an error, and the week number is still given. Errors say why in problem+json.';
  assert.equal((await keys([long])).length, 3);
});

test("a split entry's later sentence keeps its subject: every pair carries the whole entry beside its sentence", async () => {
  const s = spec();
  const long = 'The schedule lists every job the league runs this week, soonest first, with its next run time. It is an empty list, not an error, when the week has no runs, and the week number is still given.';
  s.intent.guarantees = [long, { id: 'read-only', text: 'Read-only' }];
  const units = (await builtOf(s)).evidence.guarantee;
  const second = units.filter((u) => u.id.startsWith('guarantee It is an empty list'));
  assert.ok(second.length > 0);
  for (const u of second) assert.deepEqual(u.shared, { guarantee: long.split('. ')[1], entry: long });
  assert.ok(units.filter((u) => u.id.startsWith('guarantee read-only ~ ')).every((u) => !('entry' in u.shared!)), 'an unsplit entry is its own context');
});

// Approved and recorded specs of every kind; a pair over the decision model's window would escalate unasked.
test('every pair of the fixture specs fits the decision model', async () => {
  const storePort = specFixture('store-port-v1.json');
  const wrap = (kind: ContractSpec['kind'], document: unknown): ContractSpec => ({ ...storePort, kind, document, choices: [] });
  const sessions = specFixture('sessions-api-v20.json');
  // Its grandfathered guarantees run together as one 1.8k-character entry, so each of its eight sentences carries the whole.
  const run = sessions.intent.guarantees.filter((g) => typeof g === 'string').map((g) => g.replace(/\.?$/, '.')).join(' ');
  const split = { ...sessions, intent: { ...sessions.intent, guarantees: [run] } };
  const specs = { lifecycle: specFixture('lifecycle-channel-v1.json'), storePort, deployment: specFixture('deployment-v17.json'), sessions, split, sdk: wrap('typescript', fixture('sdk-v3b.d.ts.txt')), env: wrap('json-schema', JSON.parse(fixture('env-v4.schema.json'))) };
  const checks = [{ id: 'matches_intent.meets_intent', question: contractDeclaration.checks.meets_intent.question }];
  let carried = 0;
  for (const [name, s] of Object.entries(specs)) {
    const units = (await builtOf(s)).evidence.guarantee;
    assert.ok(units.length > 0, name);
    const over = units.filter((u) => decisionModelTokens(u, checks) >= 8192).map((u) => u.id);
    assert.deepEqual(over, [], name);
    carried += units.filter((u) => u.shared?.entry).length;
  }
  assert.ok(carried > 0, 'a pair carrying its whole entry is measured');
});

test('meets_intent is the one scored check, asked of the decision model, and a spec passes only when every guarantee holds', () => {
  const checks = contractRubric.criteria.flatMap((c) => c.checks);
  assert.deepEqual(checks.filter((k) => typeof k.weight === 'number').map((k) => [k.id, k.weight]), [['meets_intent', 1]]);
  assert.deepEqual(Object.values(contractDeclaration.checks).map((c) => c.decisionModel), [PASS_ONLY], 'the decision model passes pairs; only the judge model fails one');
  assert.deepEqual(contractDeclaration.checks.meets_intent.aggregate, { name: 'grouped', groupBy: 'key', inner: 'any', outer: 'all', pass: ['yes'], none: 'none of the {n} parts checked against this guarantee defines a mechanism for it' });
  assert.deepEqual([contractRubric.version, contractRubric.threshold], [17, 1]);
});

test('with no guarantees, meets_intent has no items and counts toward neither side', async (t) => {
  const s = spec();
  s.intent.guarantees = [];
  const res = await judged(t, s, ['meets_intent'], () => 'no');
  assert.deepEqual([res.check('meets_intent').met, res.check('meets_intent').units], [null, 0]);
});
