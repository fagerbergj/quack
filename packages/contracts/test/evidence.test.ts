import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { cp, mkdir, mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test, type TestContext } from 'node:test';
import { fileURLToPath } from 'node:url';
import ts from 'typescript';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { NodeExecutionEnv } from '@earendil-works/pi-durable/env/node';
import { createModels, createProvider, envApiKeyAuth, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { typesafeSystemOneApi } from '@earendil-works/pi-ai/api/typesafe-system-one.lazy';
import { replayJudge } from '@quack/eval';
import { buildEvidence, decisionModelTokens, type Evidence, type Unit } from '@quack/judge';
import type { RecordedSession } from '@quack/sdk';
import { contractDeclaration, guideRules, kinds, manifest, recordedSpecInputs, specInputs, type ContractSpec } from '../src/index.ts';
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
    'lifecycle choice': 23, 'lifecycle definition': 31, 'store-port choice': 19, 'store-port definition': 29, 'sdk choice': 0, 'sdk definition': 83, 'env choice': 0, 'env definition': 20,
  });
});

test('a choice unit holds its decision and the rules it cites, with the whole guide as shared context; a definition unit shares the intent', async () => {
  const ev = await evidenceOf(lifecycle);
  const rules = guideRules(kinds.rest.skill);
  const errors = ev.choice.find((u) => u.id === 'choice errors')!;
  assert.deepEqual(errors.state, { choice: { ...lifecycle.choices.find((c) => c.id === 'errors'), cites: ['errors'] } });
  assert.match(rules.get('errors')!, /^Check\. Errors are `application\/problem\+json`/);
  assert.deepEqual(errors.shared, { guide: Object.fromEntries(rules) });
  assert.ok(ev.choice.every((u) => JSON.stringify(u.shared) === JSON.stringify(errors.shared)), 'one guide for every choice, so they pack together');
  assert.deepEqual(ev.definition[0].shared, { intent: lifecycle.intent });
  assert.deepEqual(Object.keys(ev.definition[0].state), ['definition'], 'the intent is sent once per request, not per definition');
  type Def = { text: unknown; decided: Array<{ choice: string }>; usedBy: string[] };
  const def = (e: Evidence, id: string) => e.definition.find((u) => u.id === `definition ${id}`)!.state.definition as unknown as Def;
  // A definition carries the choices that decide it: by pointer, or by naming it in the decision.
  const decided = (id: string) => def(ev, id).decided.map((d) => d.choice);
  assert.ok(decided('/components/schemas/Problem').includes('errors'));
  assert.ok(decided('POST /sessions/{key}/events').includes('transport'), 'an operation is decided by a choice on its path');
  const port = await evidenceOf(storePort);
  const money = def(port, '/Money');
  assert.ok(money.decided.some((d) => d.choice === 'money'));
  // And the definitions that use it, so a shared shape reads as asked for: a local $ref, or a TypeScript name.
  const usedBy = (id: string) => def(ev, id).usedBy;
  assert.ok(usedBy('/components/schemas/Problem').includes('POST /sessions/{key}/events'));
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

test('a recorded code ref under a directory too many names the file where the recording has it', async () => {
  const spec = scheduleSpecV1({ versioning: 'request', time: 'request' });
  spec.choices[1].why[1].ref = 'repo/v1/packages/sleeper/src/service.ts:85';
  const readFile = async (p: string) => (p === 'repo/packages/sleeper/src/service.ts' ? 'x\n'.repeat(90) : undefined);
  const { facts } = await inputsOf(spec, { readFile });
  const [note] = (JSON.parse(facts) as { unresolved: string[] }).unresolved.filter((u) => u.includes('repo/v1/packages/sleeper'));
  assert.equal(note, 'time-format: code ref "repo/v1/packages/sleeper/src/service.ts:85" names no file in the workspace; the file is at repo/packages/sleeper/src/service.ts, so cite it as "repo/packages/sleeper/src/service.ts:85"');
});

// The refs of prod session contracts:1811f4ba, judged live against a workspace laid out as a git clone, as that session's copy of packages/, or empty.
const SERVICE = 'packages/sleeper/src/service.ts';
async function liveNotes(t: TestContext, layout: Record<string, string>, refs: string[]): Promise<string[]> {
  const dir = await mkdtemp(join(tmpdir(), 'contracts-ws-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await mkdir(join(dir, 'artifacts'));
  for (const [at, from] of Object.entries(layout)) await mkdir(dirname(join(dir, at)), { recursive: true }).then(() => cp(join(repoRoot, from), join(dir, at)));
  const spec = scheduleSpecV1({ versioning: 'request', time: 'request' });
  spec.choices[1].why = refs.map((ref) => ({ source: 'code', text: 'upcomingRuns', ref }));
  const content = JSON.stringify(spec);
  const api = { env: new NodeExecutionEnv({ cwd: dir }), conversationId: 'c', snapshot: async () => undefined } as unknown as ToolExecutionApi;
  const { facts } = await specInputs()(api, { kind: 'contract.spec', version: 1, hash: '', content, sealedAt: 1, verdict: null }, BACKGROUND_CONTEXT);
  return (JSON.parse(facts) as { unresolved: string[] }).unresolved.filter((u) => u.startsWith('time-format: code ref '));
}

test('a live code ref resolves only to a file in the workspace, and a wrong one is told the exact ref', async (t) => {
  const notes = await liveNotes(t, { [`repo/${SERVICE}`]: SERVICE }, [`repo/${SERVICE}:64`, 'repo/sleeper/src/service.ts:64', `${SERVICE}:64`, `${join(repoRoot, SERVICE)}:64`]);
  const exact = 'names no file in the workspace; the file is at repo/packages/sleeper/src/service.ts, so cite it as "repo/packages/sleeper/src/service.ts:64"';
  assert.deepEqual(notes, [
    `time-format: code ref "repo/sleeper/src/service.ts:64" ${exact}`,
    `time-format: code ref "${SERVICE}:64" ${exact}`,
    `time-format: code ref "${join(repoRoot, SERVICE)}:64" is an absolute path; refs are paths from the workspace root, as "repo/<path in the clone>:64"`,
  ], 'an absolute path to the file outside the workspace does not resolve');
});

test('a ref resolves in the layout the session made: its packages/ copied to repo/ puts sleeper at repo/sleeper', async (t) => {
  const notes = await liveNotes(t, { 'repo/sleeper/src/service.ts': SERVICE }, ['repo/sleeper/src/service.ts:64', `${SERVICE}:64`, `repo/${SERVICE}:64`]);
  const exact = 'names no file in the workspace; the file is at repo/sleeper/src/service.ts, so cite it as "repo/sleeper/src/service.ts:64"';
  assert.deepEqual(notes, [`time-format: code ref "${SERVICE}:64" ${exact}`, `time-format: code ref "repo/${SERVICE}:64" ${exact}`]);
});

test('a ref to no file gives the exact form from the clone directory, or says to clone when there is none', async (t) => {
  const [inClone] = await liveNotes(t, { [`repo/${SERVICE}`]: SERVICE }, ['repo/packages/sleeper/src/missing.ts:3']);
  assert.equal(inClone, 'time-format: code ref "repo/packages/sleeper/src/missing.ts:3" names no file in the workspace. Refs are paths from the workspace root and your clone is repo/, so a file at packages/sleeper/src/missing.ts in the clone is cited as "repo/packages/sleeper/src/missing.ts:3"; read the clone to find where the file is');
  const [empty] = await liveNotes(t, {}, [`${SERVICE}:64`]);
  assert.equal(empty, `time-format: code ref "${SERVICE}:64" names no file in the workspace, which holds no clone: clone the repository into repo/ (git clone --depth 1 -b <branch> <url> repo) and cite a file at ${SERVICE} in it as "repo/${SERVICE}:64"`);
});

// A stand-in for Clef on its wire format: a confident yes to every bool and no to every choice, so the intent is met and no pitfall is present.
async function fakeClef(t: TestContext) {
  const asked: string[][] = [];
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    const sent = JSON.parse(body).questions as Record<string, { type: string; criteria: Record<string, string> }>;
    const questions = Object.keys(sent);
    asked.push(questions);
    const choice = (q: { criteria: Record<string, string> }) => {
      const [first, ...rest] = Object.keys(q.criteria).sort();
      return { type: 'choice', choice: first, confidence: 0.95, probabilities: Object.fromEntries([[first, 0.95], ...rest.map((k) => [k, 0.05 / rest.length])]) };
    };
    const answers = Object.fromEntries(questions.map((q) => [q, sent[q].type === 'choice' ? choice(sent[q]) : { type: 'noul', noul: 0.95 }]));
    res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ answers, usage: { input_tokens: 1, output_tokens: 0 } }));
  }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const key = process.env.QUACK_CLEF_KEY;
  process.env.QUACK_CLEF_KEY = 'none';
  t.after(() => (server.close(), key === undefined ? delete process.env.QUACK_CLEF_KEY : (process.env.QUACK_CLEF_KEY = key)));
  const baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1`;
  const clef = { id: 'clef', name: 'clef', type: 'classifier', api: 'typesafe-system-one', provider: 'clef', baseUrl, input: ['text'], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 8192 };
  return { asked, provider: createProvider({ id: 'clef', baseUrl, auth: { apiKey: envApiKeyAuth('clef', ['QUACK_CLEF_KEY']) }, models: [clef] as never, classifiers: { 'typesafe-system-one': typesafeSystemOneApi() } }) };
}

test('with Clef on, Clef answers every choice and definition unit and only meets_intent escalates to the judge model', async (t) => {
  const judge = fauxProvider({ provider: 'judge', models: [{ id: 'j' }] });
  judge.setResponses([fauxAssistantMessage(JSON.stringify({ choice: 'yes', confidence: 0.9, reason: 'every guarantee has a mechanism' }))]);
  const models = createModels();
  models.setProvider(judge.provider);
  const clef = await fakeClef(t);
  models.setProvider(clef.provider);
  const adapters = manifest.evalAdapters!;
  const content = JSON.stringify(lifecycle);
  const inputs = await inputsOf(lifecycle);
  const { rows } = await replayJudge({
    cases: [{ case: 'lifecycle', kind: 'contract.spec', checks: ['meets_intent', 'contradicts_guide', 'adds_unasked'], content, inputs }],
    arms: [{ label: 'clef', adapters, decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } }],
    repeat: 1, concurrency: 1, models, judgeModel: { provider: 'judge', modelId: 'j' },
  });
  const by = Object.fromEntries(rows.map((r) => [r.check, { met: r.met, clef: r.clef, esc: r.esc }]));
  assert.deepEqual(by, {
    meets_intent: { met: 1, clef: 0, esc: 1 },
    contradicts_guide: { met: 0, clef: 23, esc: 0 },
    adds_unasked: { met: 0, clef: 31, esc: 0 },
  });
  assert.equal(clef.asked.flat().length, 23 + 31, 'one question per unit');
  assert.ok(clef.asked.length <= 6, `units sharing a guide or an intent are packed: ${clef.asked.length} requests`);
});
