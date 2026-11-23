import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import ts from 'typescript';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { replayJudge } from '@quack/eval';
import { buildEvidence, type Evidence } from '@quack/judge';
import type { RecordedSession } from '@quack/sdk';
import { contractDeclaration, kinds, manifest, recordedSpecInputs, type ContractSpec } from '../src/index.ts';
import { fakeClef } from './clef.ts';
import { scheduleSpecV1 } from './fixtures.ts';


// Approved contracts as they were sealed: two whole specs, and two documents whose specs are not kept here.
const fixture = (name: string) => readFileSync(new URL(`fixtures-specs/${name}`, import.meta.url), 'utf8');
const lifecycle = JSON.parse(fixture('lifecycle-channel-v1.json')) as ContractSpec;
const storePort = JSON.parse(fixture('store-port-v1.json')) as ContractSpec;
const sdk = fixture('sdk-v3b.d.ts.txt');
const env = JSON.parse(fixture('env-v4.schema.json')) as Record<string, Record<string, unknown>>;

const inputsOf = (spec: ContractSpec, session: Partial<RecordedSession> = {}) => {
  const content = JSON.stringify(spec);
  const sealed = { kind: 'contract.spec', version: 1, hash: '', content, sealedAt: 10, verdict: null };
  return recordedSpecInputs({ docs: {}, artifacts: { 'contract.spec': [sealed] }, readFile: async () => undefined, ...session }, content);
};
const evidenceOf = async (spec: ContractSpec): Promise<Evidence> => (await buildEvidence(contractDeclaration, await inputsOf(spec))).evidence;
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

test("a choice's whys are the writer's account: an unanswered question, a missing file or a moved line fails nothing; a pointer into nothing does", async () => {
  const unresolved = async (spec: ContractSpec) => (JSON.parse((await inputsOf(spec)).facts) as { unresolved: string[] }).unresolved;
  const s = scheduleSpecV1({ versioning: 'never asked', time: 'rest#no-such-heading' });
  s.choices[2].why.push({ source: 'code', text: 'gone', ref: 'repo/packages/sleeper/src/missing.ts:3' }, { source: 'code', text: 'moved', ref: 'repo/packages/contracts/src/rubric.ts:99999' });
  assert.deepEqual(await unresolved(s), []);
  s.choices[0].pointer = '/info/nowhere';
  assert.deepEqual(await unresolved(s), ['versioning: pointer `/info/nowhere` is not in the document']);
});

test('the decision model settles every guarantee pair of the lifecycle spec it is sure of, and the judge model is asked nothing', async (t) => {
  const models = createModels();
  models.setProvider(fauxProvider({ provider: 'judge', models: [{ id: 'j' }] }).provider);
  const clef = await fakeClef(t, () => 'yes');
  models.setProvider(clef.provider);
  const { rows } = await replayJudge({
    cases: [{ case: 'lifecycle', kind: 'contract.spec', checks: ['meets_intent'], content: JSON.stringify(lifecycle), inputs: await inputsOf(lifecycle) }],
    arms: [{ label: 'clef', adapters: manifest.evalAdapters!, decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } }],
    repeat: 1, concurrency: 1, models, judgeModel: { provider: 'judge', modelId: 'j' },
  });
  const ev = await evidenceOf(lifecycle);
  assert.deepEqual(rows.map((r) => [r.check, r.met, r.clef, r.esc]), [['meets_intent', 1, ev.guarantee.length, 0]]);
  assert.deepEqual(Object.keys(ev), ['facts', 'guarantee'], 'no definition, purpose or non-goal scope is split');
});
