import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { test, type TestContext } from 'node:test';
import ts from 'typescript';
import { createModels, createProvider, envApiKeyAuth, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { typesafeSystemOneApi } from '@earendil-works/pi-ai/api/typesafe-system-one.lazy';
import { replayJudge } from '@quack/eval';
import { classifierTokens, type Evidence, type Unit } from '@quack/judge';
import { contractRubric, guideRules, kinds, manifest, recordedSpecEvidence, type ContractSpec } from '../src/index.ts';

// Approved contracts as they were sealed: two whole specs, and two documents whose specs are not kept here.
const fixture = (name: string) => readFileSync(new URL(`fixtures-specs/${name}`, import.meta.url), 'utf8');
const lifecycle = JSON.parse(fixture('lifecycle-channel-v1.json')) as ContractSpec;
const storePort = JSON.parse(fixture('store-port-v1.json')) as ContractSpec;
const sdk = fixture('sdk-v3b.d.ts.txt');
const env = JSON.parse(fixture('env-v4.schema.json')) as Record<string, Record<string, unknown>>;
// A bare document is judged with the store port's intent, a real one of the usual size.
const wrap = (kind: ContractSpec['kind'], document: unknown): ContractSpec => ({ ...storePort, kind, document, choices: [] });

const evidenceOf = (spec: ContractSpec) => {
  const content = JSON.stringify(spec);
  const sealed = { kind: 'contract.spec', version: 1, hash: '', content, sealedAt: 1, verdict: null };
  return recordedSpecEvidence({ docs: {}, artifacts: { 'contract.spec': [sealed] }, readFile: async () => undefined }, content);
};
const checksOf = (scope: string) => contractRubric.criteria.flatMap((c) => c.checks.filter((k) => k.scope === scope).map((k) => ({ ...k, id: `${c.id}.${k.id}` })));
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
      const over = ev[scope].filter((u) => classifierTokens(u, checksOf(scope)) >= 8192);
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
  assert.deepEqual(errors.state, { choice: lifecycle.choices.find((c) => c.id === 'errors'), cites: ['errors'] });
  assert.match(rules.get('errors')!, /^Check\. Errors are `application\/problem\+json`/);
  assert.deepEqual(errors.shared, { guide: Object.fromEntries(rules) });
  assert.ok(ev.choice.every((u) => u.shared === errors.shared), 'one guide for every choice, so they pack together');
  assert.deepEqual(ev.definition[0].shared, { intent: lifecycle.intent });
  assert.equal('intent' in ev.definition[0].state, false, 'the intent is sent once per request, not per definition');
  // A definition carries the choices that decide it: by pointer, or by naming it in the decision.
  const decided = (id: string) => (ev.definition.find((u) => u.id === `definition ${id}`)!.state.decided as Array<{ choice: string }>).map((d) => d.choice);
  assert.ok(decided('/components/schemas/Problem').includes('errors'));
  assert.ok(decided('POST /sessions/{key}/events').includes('transport'), 'an operation is decided by a choice on its path');
  const port = await evidenceOf(storePort);
  assert.ok((port.definition.find((u) => u.id === 'definition /Money')!.state.decided as Array<{ choice: string }>).some((d) => d.choice === 'money'));
});

// A stand-in for Clef on its wire format: every pitfall question is a confident no.
async function fakeClef(t: TestContext) {
  const asked: string[][] = [];
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    const questions = Object.keys(JSON.parse(body).questions);
    asked.push(questions);
    const answers = Object.fromEntries(questions.map((q) => [q, { type: 'noul', noul: 0.05 }]));
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
  const evidence = await evidenceOf(lifecycle);
  const { rows } = await replayJudge({
    cases: [{ case: 'lifecycle', kind: 'contract.spec', checks: ['meets_intent', 'contradicts_guide', 'adds_unasked'], content, evidence }],
    arms: [{ label: 'clef', adapters, classifier: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } }],
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
