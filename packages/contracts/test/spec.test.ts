import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { buildEvidence, codeResults } from '@quack/judge';
import type { Approval, RecordedSession, SealedArtifact } from '@quack/sdk';
import { contractOf, versionsOf } from '../src/app.ts';
import { contractDeclaration, contractRubric, guideRules, kinds, recordedSpecInputs, validateSpec, type ContractSpec } from '../src/index.ts';
import { scheduleSpecV1, scheduleSpecV2, SLEEPER_PAGE } from './fixtures.ts';

// The contract's own sealed spec: its document is the shape of every contract.spec, and the spec is one itself.
const CONTRACT = { key: 'contracts:d45ad598-d0a3-4302-9d5c-faf634c3911d', version: 7 };
const sealedContract = JSON.parse(readFileSync(new URL('fixtures-specs/contract-spec-v7.json', import.meta.url), 'utf8')) as ContractSpec;
const accepts = new Ajv2020({ strict: false, validateFormats: false }).compile(sealedContract.document as object);

const refs = { versioning: 'request', time: 'request' };
const v1 = scheduleSpecV1(refs);
const revision = scheduleSpecV2(refs, [SLEEPER_PAGE]);
const greenfieldV2: ContractSpec = { ...v1, version: 2 };
const compatible = { breaking: [], safe: ['every route answers as it did'] };
const shippedV1: ContractSpec = { ...v1, baseline: 'shipped', changes: [], compatibility: compatible };
const problems = (spec: unknown) => validateSpec(JSON.stringify(spec));
const without = <T extends object>(o: T, ...keys: string[]) => Object.fromEntries(Object.entries(o).filter(([k]) => !keys.includes(k)));
const breaking = (entry: object) => ({ ...revision, compatibility: { breaking: [entry], safe: [] } });

test(`the checked-in shape is ${CONTRACT.key} v${CONTRACT.version}`, () => {
  assert.deepEqual([sealedContract.title, sealedContract.version, sealedContract.baseline], ['The sealed contract.spec artifact', 7, 2]);
});

test('a greenfield draft seals at any version with no baseline, changes or compatibility', () => {
  assert.deepEqual(problems(v1), []);
  assert.deepEqual(problems(greenfieldV2), []);
  assert.deepEqual(problems({ ...greenfieldV2, version: 7 }), []);
  assert.match(problems({ ...greenfieldV2, changes: revision.changes }).join(), /measured against a baseline: name it in baseline/);
  assert.match(problems({ ...greenfieldV2, compatibility: compatible }).join(), /measured against a baseline/);
});

test('a baseline, "shipped" from version 1 or an approved version, requires changes and compatibility', () => {
  assert.deepEqual(problems({ ...v1, baseline: 'shipped' }), ['changes must list what this version changes from its baseline (shipped)', 'compatibility needs breaking (what, callers and migration) and safe']);
  assert.deepEqual(problems(shippedV1), [], 'a shipped surface may be written down unchanged');
  assert.deepEqual(problems(revision), []);
  assert.deepEqual(problems({ ...revision, changes: [] }), ['changes must list what this version changes from its baseline (1)'], 'a revision of an approved version changes something');
  for (const bad of [0, -1, 1.5, '1', 'v1', null]) assert.match(problems({ ...revision, baseline: bad }).join(), /baseline must be the number of the last approved version/, String(bad));
});

test('every breaking entry sealed now needs what, a named caller and one migration', () => {
  const entry = revision.compatibility!.breaking[0];
  assert.deepEqual(problems(breaking(without(entry, 'migration'))), ['each breaking entry needs what, at least one named caller, and migration: how those callers keep working']);
  assert.equal(problems(breaking({ ...entry, migration: ' ' })).length, 1);
  assert.equal(problems(breaking({ ...entry, callers: [] })).length, 1);
  assert.equal(problems(breaking({ ...entry, callers: SLEEPER_PAGE })).length, 1);
  assert.equal(problems(breaking({ ...without(entry, 'what'), change: entry.what })).length, 1);
});

const withFacts = (spec: ContractSpec, guarantees: unknown[], nonGoals: unknown[] = spec.intent.nonGoals) => ({ ...spec, intent: { ...spec.intent, guarantees, nonGoals } });
const fact = (text: string, id = 'a.fact') => ({ id, text });
const ACCEPTED_FACTS: Array<[string, unknown]> = [
  ['an object of 160 code points that are 320 UTF-16 units', withFacts(v1, [fact('😀'.repeat(160))])],
  ['a stored string of 2,762 characters beside an approved baseline', withFacts(revision, ['x'.repeat(2762)], ['y'.repeat(338)])],
  ['a changes row naming facts', { ...revision, changes: [{ ...revision.changes![0], facts: ['read-only'] }] }],
];

// Shape problems both refuse; the rules that depend on the baseline are the code's, which the schema leaves open.
const SHAPE_REFUSED: Array<[string, unknown]> = [
  ['a fact of 161 characters', withFacts(v1, [fact('x'.repeat(161))])],
  ['a fact of 161 code points', withFacts(revision, [fact('😀'.repeat(161))])],
  ['a fact without an id', withFacts(revision, [{ text: 'short' }])],
  ...['Reuse.Key', '1x', 'a..b', 'a.', 'a_b'].map((id): [string, unknown] => [`a fact id ${id}`, withFacts(revision, [fact('short', id)])]),
  ['a fact with empty text', withFacts(revision, [fact('')])],
  ['an empty string entry', withFacts(revision, [''])],
  ['nonGoals that are not a list', withFacts(revision, [], 'Secrets' as never)],
  ['no title', without(v1, 'title')],
  ['an unknown kind', { ...v1, kind: 'grpc' }],
  ['no choices', { ...v1, choices: [] }],
  ['a why without a known source', { ...v1, choices: [{ ...v1.choices[0], why: [{ source: 'web', text: 't', ref: 'r' }] }] }],
  ['an intent without purpose', { ...v1, intent: without(v1.intent, 'purpose') }],
  ['a typescript spec whose document is an object', { ...v1, kind: 'typescript' }],
  ['baseline 0', { ...revision, baseline: 0 }],
  ['baseline "v1"', { ...revision, baseline: 'v1' }],
  ['a breaking entry without migration', breaking(without(revision.compatibility!.breaking[0], 'migration'))],
  ['a breaking entry naming no caller', breaking({ ...revision.compatibility!.breaking[0], callers: [] })],
  ['callers as one string', breaking({ ...revision.compatibility!.breaking[0], callers: SLEEPER_PAGE })],
  ['a change row without was', { ...revision, changes: [without(revision.changes![0], 'was')] }],
  ['compatibility without safe', { ...revision, compatibility: without(revision.compatibility!, 'safe') }],
];
const CODE_REFUSED: Array<[string, unknown]> = [
  ['a baseline without changes or compatibility', { ...v1, baseline: 'shipped' }],
  ['changes without a baseline', { ...greenfieldV2, changes: revision.changes, compatibility: compatible }],
  ['an approved baseline with no change rows', { ...revision, changes: [] }],
  ['a greenfield string entry', withFacts(v1, ['Read-only'])],
  ['a string entry with baseline "shipped"', withFacts(shippedV1, v1.intent.guarantees, ['Secrets'])],
  ['an id used in both lists', withFacts(v1, [fact('one')], [fact('two')])],
  ['an id used twice in one list', withFacts(revision, [fact('one'), fact('two')])],
];

test('the seal requires an id on every entry with no baseline or "shipped", and unique ids across both lists', () => {
  const noId = 'facts are not decomposed: guarantees[0]: no id — give it one';
  assert.deepEqual(problems(withFacts(v1, ['Read-only'])), [noId]);
  assert.deepEqual(problems(withFacts(shippedV1, ['Read-only'])), [noId]);
  assert.deepEqual(problems(withFacts(revision, ['Read-only'])), [], 'beside an approved baseline the judge decides whether a string is unchanged');
  assert.deepEqual(problems(withFacts(v1, [fact('one')], [fact('two')])), ["facts are not decomposed: nonGoals[0]: duplicate id — 'a.fact' is used more than once, so give each entry its own id"]);
});

test('the seal check and the contract schema agree on the specs both accept and the shapes both refuse', () => {
  const examples = (sealedContract.document as { examples: unknown[] }).examples.map((e, i): [string, unknown] => [`example ${i}`, e]);
  for (const [what, spec] of [...[v1, greenfieldV2, shippedV1, revision, sealedContract].map((s): [string, unknown] => [s.title, s]), ...examples, ...ACCEPTED_FACTS]) {
    assert.deepEqual(problems(spec), [], what);
    assert.equal(accepts(spec), true, `${what}: ${JSON.stringify(accepts.errors)}`);
  }
  for (const [what, spec] of SHAPE_REFUSED) {
    assert.notDeepEqual(problems(spec), [], `the seal accepted ${what}`);
    assert.equal(accepts(spec), false, `the schema accepted ${what}`);
  }
  for (const [what, spec] of CODE_REFUSED) {
    assert.notDeepEqual(problems(spec), [], `the seal accepted ${what}`);
    assert.equal(accepts(spec), true, `the schema now decides ${what}; move the rule out of the code`);
  }
});

// A session as recorded: spec artifacts (verdict passed unless null), approval records naming their artifact, and the approvals.
const sealed = (version: number, spec: unknown, at: number, passed: boolean | null = true): SealedArtifact =>
  ({ kind: 'contract.spec', version, hash: `h${version}`, content: JSON.stringify(spec), sealedAt: at, verdict: passed === null ? null : ({ passed } as never) });
const approvedRecord = (spec: number, artifact: number, at: number): SealedArtifact =>
  ({ kind: 'contract.approved', version: 1, hash: 'r', content: JSON.stringify({ version: spec, artifact, hash: `h${artifact}`, note: '', approval: 'a1' }), sealedAt: at, verdict: null });
const approval: Approval = { tool: 'propose_contract', args: {}, requestedAt: 15, verdict: { decision: 'approve', by: 'jf', reason: null, at: 16 }, executing: false, done: true };

async function judgedFacts(specs: SealedArtifact[], approved: SealedArtifact[] = []) {
  const session = { docs: { 'quack.approvals': { items: { a1: approval } } }, artifacts: { 'contract.spec': specs, 'contract.approved': approved }, readFile: async () => undefined } as unknown as RecordedSession;
  const judged = specs.at(-1)!.content;
  const inputs = await recordedSpecInputs(session, judged);
  const results = codeResults(contractRubric, judged, (await buildEvidence(contractDeclaration, inputs)).evidence);
  return { breaking: (JSON.parse(inputs.facts) as { breaking: unknown[] }).breaking, gate: results.find((r) => r.id === 'baseline_matches')!, results };
}

test('a revision of an approved contract must name that version as its baseline', async () => {
  const v1Approved = [approvedRecord(1, 1, 20)];
  const ok = await judgedFacts([sealed(1, v1, 10), sealed(2, revision, 30, null)], v1Approved);
  assert.equal(ok.gate.met, 1);
  assert.ok(ok.breaking.length > 0, 'the break is found against approved version 1');
  for (const spec of [greenfieldV2, { ...revision, baseline: 'shipped' }, { ...revision, baseline: 3 }]) {
    const { gate } = await judgedFacts([sealed(1, v1, 10), sealed(2, spec, 30, null)], v1Approved);
    assert.deepEqual([gate.met, gate.notes], [0, ['version 1 of this contract is approved, so set baseline to 1 and write changes and compatibility against it']], JSON.stringify((spec as ContractSpec).baseline));
  }
});

test('facts_decomposed compares a revision\'s facts with the approved spec\'s and names each offender', async () => {
  const decomposed = async (spec: unknown, approved = [approvedRecord(1, 1, 20)]) => (await judgedFacts([sealed(1, v1, 10), sealed(2, spec, 30, null)], approved)).results.find((r) => r.id === 'facts_decomposed')!;
  assert.equal((await decomposed(revision)).met, 1);
  const edited = withFacts(revision, [fact('Read-only, always', 'read-only'), v1.intent.guarantees[1]]);
  assert.deepEqual([(await decomposed(edited)).met, (await decomposed(edited)).notes], [0, ["facts are not decomposed: guarantees[0]: id named in no changes row's facts — 'read-only' was added with new text, changed or removed, so name it in the row that touches the list."]]);
  assert.equal((await decomposed({ ...edited, changes: [{ ...revision.changes![0], facts: ['read-only'] }] })).met, 1);
  const stringsOnly = withFacts(revision, ['Read-only']);
  assert.match((await decomposed(stringsOnly, [])).notes[0], /^facts are not decomposed: guarantees\[0\]: no id — give it one\.$/, 'with nothing approved every entry needs an id');
  assert.equal(contractRubric.criteria.find((c) => c.id === 'sound')!.checks.find((c) => c.id === 'facts_decomposed')!.weight, 'gate');
});

// #2391: contract.spec numbers by gate-passed versions and "approval does not affect numbering", so a withdrawn proposal's number stays used.
test('a passed version whose proposal was withdrawn still uses its number, and a failed one leaves it free', async () => {
  const withdrawn: Approval = { tool: 'propose_contract', args: { version: 1, artifact: 1, hash: 'h1' }, requestedAt: 11, verdict: null, executing: false, done: false, withdrawn: { at: 12, reason: 'task_ended', supersededBy: null } };
  const matches = async (version: number, passed = true) => {
    const next = sealed(2, { ...v1, version }, 30, null);
    const session = { docs: { 'quack.approvals': { items: { p1: withdrawn } } }, artifacts: { 'contract.spec': [sealed(1, v1, 10, passed), next] }, readFile: async () => undefined } as unknown as RecordedSession;
    const inputs = await recordedSpecInputs(session, next.content);
    const r = codeResults(contractRubric, next.content, (await buildEvidence(contractDeclaration, inputs)).evidence).find((x) => x.id === 'version_matches')!;
    return [r.met, r.notes];
  };
  assert.deepEqual(await matches(1), [0, ['version is 1; this is spec version 2 (one more than the last delivered), so set version to 2']]);
  assert.deepEqual(await matches(2), [1, []]);
  assert.deepEqual(await matches(1, false), [1, []], 'a failed revision reuses its number');
});

test('with nothing approved, a numbered baseline is refused, and the judge finds no breaks: a passed draft is not a baseline', async () => {
  const numbered = await judgedFacts([sealed(1, v1, 10), sealed(2, revision, 30, null)]);
  assert.equal(numbered.gate.met, 0);
  assert.match(numbered.gate.notes[0], /baseline is 1, but no version of this contract is approved/);
  for (const spec of [greenfieldV2, { ...revision, baseline: 'shipped' }]) {
    const { gate, breaking: found } = await judgedFacts([sealed(1, v1, 10), sealed(2, spec, 30, null)]);
    assert.equal(gate.met, 1);
    assert.deepEqual(found, [], 'v2 changes the run time type, but no approved spec stands to diff it against');
  }
});

// Real shapes from prod: `change` for what, `choice`/`was`/`now` on an entry, callers in one string, and no migration.
const stored = {
  ...greenfieldV2,
  changes: [{ choice: 'time-format', was: 'ISO', now: 'epoch', why: 'asked' }],
  compatibility: { breaking: [
    { change: 'agent.json is closed (additionalProperties false)', callers: 'The 13 bundles under agents/*/agent.json', migration: 'Delete unknown keys' },
    { choice: 'errors', was: 'error strings', now: 'problem+json', callers: ['repo/packages/web/src/api.ts:28'], note: 'n', why: 'w' },
    { what: 'runs[].at becomes an integer', callers: [SLEEPER_PAGE] },
  ], safe: ['week is unchanged'] },
};

test('specs sealed before the baseline rule still read through the judge and the app', async () => {
  const { gate, results } = await judgedFacts([sealed(1, v1, 10), sealed(2, stored, 30, null)]);
  assert.equal(gate.met, 1);
  assert.equal(results.find((r) => r.id === 'breaking_named')!.met, 1);
  const artifacts = [{ kind: 'contract.spec', version: 1, content: JSON.stringify(stored) }];
  assert.deepEqual(contractOf('contracts:x', undefined, artifacts, []), { key: 'contracts:x', title: v1.title, kind: 'rest', draft: 2, pendingApproval: false });
  assert.deepEqual(versionsOf(artifacts, []), [{ artifact: 1, version: 2, status: 'draft' }]);
});

test('every kind guide and the contracts prompt carry the one-fact rule word for word as the contract records it', () => {
  const decision = sealedContract.choices.find((c) => c.id === 'authoring-rule')!.decision;
  const rule = /carries it too: '(.+)'$/.exec(decision)![1]!;
  for (const kind of Object.values(kinds)) assert.equal(guideRules(kind.skill).get('one fact per entry'), rule);
  assert.ok(readFileSync(new URL('../../../agents/contracts/prompt.md', import.meta.url), 'utf8').includes(`\n${rule}\n`));
});
