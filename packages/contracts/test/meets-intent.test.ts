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
  const missing = await judged(t, spec(), ['meets_intent'], (q) => (q.includes(MECHANISM) && q.includes(OP) && !q.includes('read-only') ? 'yes' : 'no'));
  const failed = missing.check('meets_intent');
  assert.equal(failed.met, false);
  assert.equal(failed.notes.length, 1);
  assert.match(failed.notes[0], /^read-only: guarantee read-only ~ .+: /);
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
