import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { cp, mkdir, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test, type TestContext } from 'node:test';
import { fileURLToPath } from 'node:url';
import ts from 'typescript';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { NodeExecutionEnv } from '@earendil-works/pi-durable/env/node';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { replayJudge } from '@quack/eval';
import { buildEvidence, decisionModelTokens, registerSelector, type Declaration, type Evidence, type Unit } from '@quack/judge';
import type { RecordedSession } from '@quack/sdk';
import { contractDeclaration, guideRules, kinds, manifest, recordedSpecInputs, specInputs, type ContractSpec } from '../src/index.ts';
import { fakeClef } from './clef.ts';
import { scheduleSpecV1 } from './fixtures.ts';

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '../../..');

// Approved contracts as they were sealed: two whole specs, and two documents whose specs are not kept here.
const fixture = (name: string) => readFileSync(new URL(`fixtures-specs/${name}`, import.meta.url), 'utf8');
const lifecycle = JSON.parse(fixture('lifecycle-channel-v1.json')) as ContractSpec;
const storePort = JSON.parse(fixture('store-port-v1.json')) as ContractSpec;
const sdk = fixture('sdk-v3b.d.ts.txt');
const env = JSON.parse(fixture('env-v4.schema.json')) as Record<string, Record<string, unknown>>;
// A bare document is judged with the store port's intent, a real one of the usual size.
const wrap = (kind: ContractSpec['kind'], document: unknown): ContractSpec => ({ ...storePort, kind, document, choices: [] });

const inputsOf = (spec: ContractSpec, session: Partial<RecordedSession> = {}) => {
  const content = JSON.stringify(spec);
  const sealed = { kind: 'contract.spec', version: 1, hash: '', content, sealedAt: 10, verdict: null };
  return recordedSpecInputs({ docs: {}, artifacts: { 'contract.spec': [sealed] }, readFile: async () => undefined, ...session }, content);
};
const evidenceOf = async (spec: ContractSpec): Promise<Evidence> => (await buildEvidence(contractDeclaration, await inputsOf(spec))).evidence;
const checksOf = (scope: string) => Object.entries(contractDeclaration.checks).filter(([, d]) => d.scope === scope).map(([id, d]) => ({ id: `matches_intent.${id}`, question: d.question }));
const ids = (units: Unit[]) => units.map((u) => u.id);
// The definition splitter's chunk metadata, shown as context, to read what code settles a definition by.
registerSelector('test.meta', (_i, c) => JSON.stringify(c.meta));
const metaDecl: Declaration = { scopes: { definition: { split: contractDeclaration.scopes.definition.split, context: { meta: { name: 'test.meta' } } } }, checks: {} };
const noSpace = (s: string) => s.replace(/\s+/g, '');

test('openapi: one definition per operation and per named component, and the rest of the document apart', () => {
  const doc = lifecycle.document as { paths: Record<string, object>; components: Record<string, object> };
  const defs = kinds.rest.definitions(doc);
  const ops = Object.entries(doc.paths).flatMap(([p, item]) => Object.keys(item).filter((m) => m !== 'parameters').map((m) => `${m.toUpperCase()} ${p}`));
  const components = Object.entries(doc.components).flatMap(([s, entries]) => Object.keys(entries).map((n) => `/components/${s}/${n}`));
  assert.deepEqual(defs.map((d) => d.id), [...ops, ...components, '(rest of the document)']);
  assert.equal(defs.length, 31);
  assert.deepEqual(defs.find((d) => d.id === 'POST /sessions/{key}/events')!.definition, doc.paths['/sessions/{key}/events']['post' as never]);
  assert.deepEqual(Object.keys(defs.at(-1)!.definition as object), ['openapi', 'info', 'tags', 'security']);
});

test('json-schema: one definition per root property, pattern property and $defs entry, and the rest of the root apart', () => {
  const defs = kinds['json-schema'].definitions(env);
  const expected = ['properties', 'patternProperties', '$defs'].flatMap((k) => Object.keys(env[k]).map((n) => `/${k}/${n}`));
  assert.deepEqual(defs.map((d) => d.id), [...expected, '(rest of the document)']);
  assert.equal(defs.length, 20);
  assert.deepEqual(defs.find((d) => d.id === '/$defs/session')!.definition, env.$defs.session);
  assert.deepEqual(Object.keys(defs.at(-1)!.definition as object), ['$schema', '$id', 'title', 'description', 'type', 'required', 'additionalProperties', 'examples']);
});

test('typescript: one definition per declaration with its TSDoc, and together with the rest they hold the whole file', () => {
  for (const [doc, n] of [[sdk, 83], [storePort.document as string, 29]] as const) {
    const defs = kinds.typescript.definitions(doc);
    assert.equal(defs.length, n);
    const sf = ts.createSourceFile('x.d.ts', doc, ts.ScriptTarget.ES2022, true);
    const exported = sf.statements.filter((s) => ts.getModifiers(s as ts.HasModifiers)?.some((m) => m.kind === ts.SyntaxKind.ExportKeyword));
    for (const s of exported) assert.ok(defs.some((d) => (d.definition as string).includes(s.getText(sf))), s.getText(sf).slice(0, 60));
    assert.equal(noSpace(defs.map((d) => d.definition).join('')).length, noSpace(doc).length, 'nothing is lost or repeated');
  }
  const money = kinds.typescript.definitions(storePort.document).find((d) => d.id === '/Money')!.definition as string;
  assert.match(money, /^\/\*\*[\s\S]*export type Money = /, 'the TSDoc goes with its declaration');
});

test('every choice and definition unit of the approved contracts fits the decision model', async () => {
  const counts: Record<string, number> = {};
  for (const [name, spec] of [['lifecycle', lifecycle], ['store-port', storePort], ['sdk', wrap('typescript', sdk)], ['env', wrap('json-schema', env)]] as const) {
    const ev: Evidence = await evidenceOf(spec);
    for (const scope of ['choice', 'definition']) {
      const over = ev[scope].filter((u) => decisionModelTokens(u, checksOf(scope)) >= 8192);
      assert.deepEqual(ids(over), [], `${name} ${scope}`);
      counts[`${name} ${scope}`] = ev[scope].length;
    }
  }
  assert.deepEqual(counts, {
    'lifecycle choice': 23, 'lifecycle definition': 30, 'store-port choice': 19, 'store-port definition': 28, 'sdk choice': 0, 'sdk definition': 82, 'env choice': 0, 'env definition': 19,
  });
});

test('a choice unit holds its decision and the rules it cites, with the whole guide as shared context; a definition unit shares the kind and the purpose', async () => {
  const ev = await evidenceOf(lifecycle);
  const rules = guideRules(kinds.rest.skill);
  const errors = ev.choice.find((u) => u.id === 'choice errors')!;
  assert.deepEqual(errors.state, { choice: { ...lifecycle.choices.find((c) => c.id === 'errors'), cites: ['errors'] } });
  assert.match(rules.get('errors')!, /^Check\. Errors are `application\/problem\+json`/);
  assert.deepEqual(errors.shared, { guide: Object.fromEntries(rules) });
  assert.ok(ev.choice.every((u) => JSON.stringify(u.shared) === JSON.stringify(errors.shared)), 'one guide for every choice, so they pack together');
  assert.deepEqual(ev.definition[0].shared, { kind: 'rest', purpose: lifecycle.intent.purpose }, 'every definition shares only the kind and the purpose, so they pack together');
  assert.deepEqual(Object.keys(ev.definition[0].state), ['definition', 'consumersAndGuarantees'], 'no definition carries the whole intent');
  const covered = async (spec: ContractSpec, id: string) => {
    const { evidence } = await buildEvidence(metaDecl, await inputsOf(spec));
    return evidence.definition.find((u) => u.id === `definition ${id}`)!.state.meta as unknown as { decided: string[]; usedBy: string[] };
  };
  // A definition's chunk carries the choices that decide it: by pointer, or by naming it in the decision.
  const problem = await covered(lifecycle, '/components/schemas/Problem');
  assert.ok(problem.decided.includes('errors'));
  assert.ok((await covered(lifecycle, 'POST /sessions/{key}/events')).decided.includes('transport'), 'an operation is decided by a choice on its path');
  const money = await covered(storePort, '/Money');
  assert.ok(money.decided.includes('money'));
  // And the definitions that use it, so a shared shape reads as asked for: a local $ref, or a TypeScript name.
  assert.ok(problem.usedBy.includes('POST /sessions/{key}/events'));
  assert.ok(money.usedBy.includes('/SessionCatalogStore'));
});

test('an answer is cited by the question as asked or by its ask_question id, once answered before the seal', async () => {
  const asked = 'Keep the existing unversioned route, or add /v1?';
  const answered = (at: number) => ({ question: asked, options: [], evidence: null, askedAt: 1, answer: { choice: 'Keep it', by: 'jf', at }, closed: false });
  const unresolved = async (ref: string, at: number) => {
    const docs = { 'quack.questions': { items: { '7:versioning': answered(at) } } } as never;
    const { facts } = await inputsOf(scheduleSpecV1({ versioning: ref, time: 'request' }), { docs });
    return (JSON.parse(facts) as { unresolved: string[] }).unresolved.filter((u) => / answer ref /.test(u));
  };
  assert.deepEqual(await unresolved(asked, 5), []);
  assert.deepEqual(await unresolved('versioning', 5), []);
  assert.deepEqual(await unresolved('versioning', 11), ['versioning: answer ref "versioning" does not resolve'], 'answered after the seal');
  assert.deepEqual(await unresolved('7', 5), ['versioning: answer ref "7" does not resolve'], 'the task part of the key is not an id');
});

// The refs prod sessions 14053ed0, 608df011 and b66206df cited in rounds the gate failed.
test('a guide ref names any guide heading, whatever its case or dashes, and a miss is told the headings', async () => {
  const unresolved = async (ref: string) => {
    const spec = scheduleSpecV1({ versioning: 'request', time: 'request' });
    spec.choices[0].why[1].ref = ref;
    const { facts } = await inputsOf(spec);
    return (JSON.parse(facts) as { unresolved: string[] }).unresolved.filter((u) => / guide ref /.test(u));
  };
  for (const ref of ['rest#one-fact-per-entry', 'rest#One fact per entry', 'rest#intent-first', 'rest#Intent first', 'status-codes']) assert.deepEqual(await unresolved(ref), [], ref);
  const [miss] = await unresolved('rest#intent-first/one-fact-per-entry');
  assert.match(miss, /^versioning: guide ref "rest#intent-first\/one-fact-per-entry" does not resolve; the guide's headings are "Intent first", "One fact per entry", "document", /);
});

test('a recorded code ref under a directory too many names the file where the recording has it', async () => {
  const spec = scheduleSpecV1({ versioning: 'request', time: 'request' });
  spec.choices[1].why[1].ref = 'repo/v1/packages/sleeper/src/service.ts:85';
  const readFile = async (p: string) => (p === 'repo/packages/sleeper/src/service.ts' ? 'x\n'.repeat(90) : undefined);
  const { facts } = await inputsOf(spec, { readFile });
  const [note] = (JSON.parse(facts) as { unresolved: string[] }).unresolved.filter((u) => u.includes('repo/v1/packages/sleeper'));
  assert.equal(note, 'time-format: code ref "repo/v1/packages/sleeper/src/service.ts:85" names no file in the workspace; the file is at repo/packages/sleeper/src/service.ts, so cite it as "repo/packages/sleeper/src/service.ts:85"');
});

// The refs of prod session contracts:1811f4ba, judged live against a workspace laid out as a git clone, as that session's copy of packages/, or empty.
// A layout entry copies a file of this repo there, or with '' makes an empty directory (a clone's .git, a stray __pycache__).
const SERVICE = 'packages/sleeper/src/service.ts';
const GIT = { 'repo/.git': '' };
async function liveNotes(t: TestContext, layout: Record<string, string>, refs: string[]): Promise<string[]> {
  const dir = await mkdtemp(join(tmpdir(), 'contracts-ws-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await mkdir(join(dir, 'artifacts'));
  for (const [at, from] of Object.entries(layout)) await mkdir(dirname(join(dir, at)), { recursive: true }).then(() => (from ? cp(join(repoRoot, from), join(dir, at)) : mkdir(join(dir, at))));
  const spec = scheduleSpecV1({ versioning: 'request', time: 'request' });
  spec.choices[1].why = refs.map((ref) => ({ source: 'code', text: 'upcomingRuns', ref }));
  const content = JSON.stringify(spec);
  const api = { env: new NodeExecutionEnv({ cwd: dir }), conversationId: 'c', snapshot: async () => undefined } as unknown as ToolExecutionApi;
  const { facts } = await specInputs()(api, { kind: 'contract.spec', version: 1, hash: '', content, sealedAt: 1, verdict: null }, BACKGROUND_CONTEXT);
  return (JSON.parse(facts) as { unresolved: string[] }).unresolved.filter((u) => u.startsWith('time-format: code ref '));
}

test('a live code ref resolves only to a file in the workspace, and a wrong one is told the exact ref', async (t) => {
  const notes = await liveNotes(t, { ...GIT, [`repo/${SERVICE}`]: SERVICE }, [`repo/${SERVICE}:64`, 'repo/sleeper/src/service.ts:64', `${SERVICE}:64`, `${join(repoRoot, SERVICE)}:64`]);
  const exact = 'names no file in the workspace; the file is at repo/packages/sleeper/src/service.ts, so cite it as "repo/packages/sleeper/src/service.ts:64"';
  assert.deepEqual(notes, [
    `time-format: code ref "repo/sleeper/src/service.ts:64" ${exact}`,
    `time-format: code ref "${SERVICE}:64" ${exact}`,
    `time-format: code ref "${join(repoRoot, SERVICE)}:64" is an absolute path; refs are paths from the workspace root, as "repo/<path in the clone>:64"`,
  ], 'an absolute path to the file outside the workspace does not resolve');
});

test('a ref resolves in the layout the session made: its packages/ copied to repo/ puts sleeper at repo/sleeper', async (t) => {
  const notes = await liveNotes(t, { ...GIT, 'repo/sleeper/src/service.ts': SERVICE }, ['repo/sleeper/src/service.ts:64', `${SERVICE}:64`, `repo/${SERVICE}:64`]);
  const exact = 'names no file in the workspace; the file is at repo/sleeper/src/service.ts, so cite it as "repo/sleeper/src/service.ts:64"';
  assert.deepEqual(notes, [`time-format: code ref "${SERVICE}:64" ${exact}`, `time-format: code ref "repo/${SERVICE}:64" ${exact}`]);
});

test('a ref to no file gives the exact form from the clone directory, or says to clone when there is none', async (t) => {
  const [inClone] = await liveNotes(t, { ...GIT, [`repo/${SERVICE}`]: SERVICE }, ['repo/packages/sleeper/src/missing.ts:3']);
  assert.equal(inClone, 'time-format: code ref "repo/packages/sleeper/src/missing.ts:3" names no file in the workspace. Refs are paths from the workspace root and your clone is repo/, so a file at packages/sleeper/src/missing.ts in the clone is cited as "repo/packages/sleeper/src/missing.ts:3"; read the clone to find where the file is');
  const [empty] = await liveNotes(t, {}, [`${SERVICE}:64`]);
  assert.equal(empty, `time-format: code ref "${SERVICE}:64" names no file in the workspace, which holds no clone: clone the repository into repo/ (git clone --depth 1 -b <branch> <url> repo) and cite a file at ${SERVICE} in it as "repo/${SERVICE}:64"`);
});

// #2380: the session's python left a __pycache__/ in a workspace with no clone, and 38 refs carried from an older version cited app/.
test('only a git repository is the clone: a stray directory gets the no-clone hint, and an app/ ref is re-anchored to repo/', async (t) => {
  const APP = 'app/packages/sleeper/src/service.ts:64';
  const stray = await liveNotes(t, { __pycache__: '', 'node_modules/x/index.js': SERVICE }, [`${SERVICE}:64`, APP]);
  assert.deepEqual(stray, [
    `time-format: code ref "${SERVICE}:64" names no file in the workspace, which holds no clone: clone the repository into repo/ (git clone --depth 1 -b <branch> <url> repo) and cite a file at ${SERVICE} in it as "repo/${SERVICE}:64"`,
    `time-format: code ref "${APP}" cites app/, the old session image's tree, which no longer resolves: re-anchor it to the clone, as "repo/${SERVICE}:64", once the repository is cloned into repo/ (git clone --depth 1 -b <branch> <url> repo)`,
  ]);
  const cloned = await liveNotes(t, { __pycache__: '', ...GIT, [`repo/${SERVICE}`]: SERVICE }, [APP, 'app/packages/sleeper/src/gone.ts:3']);
  assert.deepEqual(cloned, [
    `time-format: code ref "${APP}" cites app/, the old session image's tree, which no longer resolves: re-anchor it to the clone, as "repo/${SERVICE}:64"`,
    'time-format: code ref "app/packages/sleeper/src/gone.ts:3" cites app/, the old session image\'s tree, which no longer resolves: re-anchor it to the clone, as "repo/packages/sleeper/src/gone.ts:3"',
  ]);
});

test('with Clef on, Clef answers every choice and every definition code did not settle, and the judge model every guarantee', async (t) => {
  const judge = fauxProvider({ provider: 'judge', models: [{ id: 'j' }] });
  judge.setResponses(Array.from({ length: 20 }, () => () => fauxAssistantMessage(JSON.stringify({ choice: 'yes', confidence: 0.9, reason: 'the evidence shows it' }))));
  const models = createModels();
  models.setProvider(judge.provider);
  // Every guarantee holds, every definition is named by an intent entry and nothing departs, so no pitfall is present.
  const clef = await fakeClef(t, (q) => (q.includes('named or implied') ? 'yes' : 'no'));
  models.setProvider(clef.provider);
  const adapters = manifest.evalAdapters!;
  const content = JSON.stringify(lifecycle);
  const inputs = await inputsOf(lifecycle);
  const { rows } = await replayJudge({
    cases: [{ case: 'lifecycle', kind: 'contract.spec', checks: ['meets_intent', 'contradicts_guide', 'adds_unasked', 'implements_non_goal'], content, inputs }],
    arms: [{ label: 'clef', adapters, decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } }],
    repeat: 1, concurrency: 1, models, judgeModel: { provider: 'judge', modelId: 'j' },
  });
  const by = Object.fromEntries(rows.map((r) => [r.check, { met: r.met, clef: r.clef, esc: r.esc }]));
  const ev = await evidenceOf(lifecycle);
  const asked = ev.definition.filter((u) => !u.related?.covered).length;
  assert.deepEqual(by, {
    meets_intent: { met: 1, clef: 0, esc: ev.guarantee.length },
    contradicts_guide: { met: 0, clef: 23, esc: 0 },
    adds_unasked: { met: 0, clef: asked, esc: 0 },
    implements_non_goal: { met: 0, clef: 0, esc: 0 },
  });
  const pairs = ev.nonGoal.filter((u) => !u.related?.held).length;
  assert.deepEqual([asked, pairs, ev.nonGoal.length], [0, 0, 30], 'a choice covers or another definition uses each of the 30 definitions, so code settles them all and no pair is asked');
  assert.equal(clef.asked.flat().length, 23 + asked + pairs, 'one question per unit, and none for a guarantee');
  assert.ok(clef.asked.length <= 6, `units sharing a guide or the kind are packed: ${clef.asked.length} requests`);
});
