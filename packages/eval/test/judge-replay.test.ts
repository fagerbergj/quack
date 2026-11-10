import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { join } from 'node:path';
import { test, type TestContext } from 'node:test';
import { createModels, createProvider, envApiKeyAuth, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { typesafeSystemOneApi } from '@earendil-works/pi-ai/api/typesafe-system-one.lazy';
import type { Rubric } from '@quack/judge';
import type { EvalAdapter } from '@quack/sdk';
import { packagesTar, replayJudge, table, type Arm, type Case } from '../src/index.ts';

const rubric = (question: string): Rubric => ({
  name: 'acme', version: 1, threshold: 0.5,
  criteria: [{ id: 'c', checks: [{ id: 'helpful', label: 'L helpful', weight: 2, scope: 'note', question }, { id: 'other', label: 'L other', weight: 1, scope: 'gap', question: 'Unwatched?' }] }],
});
const adapter = (question: string): EvalAdapter => ({ kind: 'acme.note', judge: { rubric: rubric(question), evidence: async () => undefined } });
const arms: Arm[] = [{ label: 'base', adapters: [adapter('Is it helpful?')] }, { label: 'cand', adapters: [adapter('Does it help a reader act?')] }];
const cases: Case[] = ['a', 'b'].map((c) => ({ case: c, kind: 'acme.note', checks: ['helpful'], content: c, evidence: { note: [{ id: c, state: { text: c } }] } }));
const answer = (yes: boolean) => fauxAssistantMessage(JSON.stringify({ choice: yes ? 'yes' : 'no', confidence: 0.9, reason: 'because' }));
const judgeModel = { provider: 'judge', modelId: 'j' };

function judgeModels(answers: boolean[]) {
  const faux = fauxProvider({ provider: 'judge', models: [{ id: 'j' }] });
  faux.setResponses(answers.map(answer));
  const models = createModels();
  models.setProvider(faux.provider);
  return models;
}

test('two cases, two arms and repeat 2 give the met values of each repeat for the watched check', async () => {
  // At concurrency 1 the judge is asked in order: repeat 1 (base a, base b, cand a, cand b), then repeat 2.
  const rows = await replayJudge({ cases, arms, repeat: 2, concurrency: 1, models: judgeModels([true, false, true, true, true, false, true, true]), judgeModel });
  const out = table(rows, ['base', 'cand']).split('\n');
  assert.deepEqual(out.slice(0, 4), [
    '| case | check | met base | met cand | clef/esc base | clef/esc cand |',
    '| --- | --- | --- | --- | --- | --- |',
    '| a | helpful | 1 1 | 1 1 | 0/2 | 0/2 |',
    '| b | helpful | 0 0 | 1 1 | 0/2 | 0/2 |',
  ]);
  assert.match(out[5], /^judge seconds per case: base median \d+\.\d p90 \d+\.\d, cand median \d+\.\d p90 \d+\.\d$/);
  assert.deepEqual(out.slice(7), ['disagreements (case check: met per repeat by arm, then the notes each arm gave):', '- b helpful: base 0 0; cand 1 1', '  - base: b: because'], 'a, where the arms agree, is not listed');
});

// A stand-in for Clef on its wire format, so an arm's classifier is really asked.
async function fakeClef(t: TestContext, probability: number) {
  const server = createServer(async (req, res) => {
    for await (const _ of req);
    res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ answers: { q0: { type: 'noul', noul: probability } }, usage: { input_tokens: 1, output_tokens: 0 } }));
  }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const key = process.env.QUACK_CLEF_KEY;
  process.env.QUACK_CLEF_KEY = 'none';
  t.after(() => (server.close(), key === undefined ? delete process.env.QUACK_CLEF_KEY : (process.env.QUACK_CLEF_KEY = key)));
  const baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1`;
  const clef = { id: 'clef', name: 'clef', type: 'classifier', api: 'typesafe-system-one', provider: 'clef', baseUrl, input: ['text'], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 8192 };
  return createProvider({ id: 'clef', baseUrl, auth: { apiKey: envApiKeyAuth('clef', ['QUACK_CLEF_KEY']) }, models: [clef] as never, classifiers: { 'typesafe-system-one': typesafeSystemOneApi() } });
}

test('arms that differ only in classifier: the classifier arm answers from Clef, the other escalates, and the disagreement is listed', async (t) => {
  const models = judgeModels([true]);
  models.setProvider(await fakeClef(t, 0.05));
  const same = adapter('Is it helpful?');
  const result = await replayJudge({ cases: [cases[0]], arms: [{ label: 'base', adapters: [same] }, { label: 'clef', adapters: [same], classifier: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } }], repeat: 1, concurrency: 1, models, judgeModel });
  const out = table(result, ['base', 'clef']).split('\n');
  assert.equal(out[2], '| a | helpful | 1 | 0 | 0/1 | 1/0 |');
  assert.equal(out[7], '- a helpful: base 1; clef 0');
});

test('an adapter that cannot rebuild a case declines it with its reason, and the other cases are still judged', async () => {
  const picky: EvalAdapter = { kind: 'acme.note', judge: { rubric: rubric('Is it helpful?'), evidence: async (_s, content) => { throw new Error(`the recording of ${content} keeps no facts`); } } };
  const bare = cases.map(({ evidence: _e, ...c }) => c);
  const result = await replayJudge({ cases: [cases[0], bare[1]], arms: [{ label: 'base', adapters: [picky] }], repeat: 1, concurrency: 1, models: judgeModels([true]), judgeModel });
  assert.deepEqual(result.declined, [{ arm: 'base', case: 'b', reason: 'the recording of b keeps no facts' }]);
  assert.deepEqual(table(result, ['base']).split('\n').slice(-2), ['declined:', '- base b: the recording of b keeps no facts']);
  await assert.rejects(replayJudge({ cases: bare, arms: [{ label: 'base', adapters: [picky] }], repeat: 1, concurrency: 1, models: judgeModels([]), judgeModel }), /every case was declined: base a: the recording of a keeps no facts; base b/);
});

test('a watched check no rubric has is an error, and a check with no unit prints null', async () => {
  await assert.rejects(replayJudge({ cases: [{ ...cases[0], checks: ['nope'] }], arms, repeat: 1, concurrency: 1, models: judgeModels([]), judgeModel }), /no check nope/);
  const gap = { ...cases[0], checks: ['c.other'] };
  const rows = await replayJudge({ cases: [gap], arms: [arms[0]], repeat: 1, concurrency: 1, models: judgeModels([]), judgeModel });
  assert.match(table(rows, ['base']), /\| a \| other \| null \| 0\/0 \|/);
});

test('a case naming units judges only those units of the evidence the adapter built', async () => {
  const two: EvalAdapter = { kind: 'acme.note', judge: { rubric: rubric('Is it helpful?'), evidence: async () => ({ note: [{ id: 'keep', state: {} }, { id: 'drop', state: {} }] }) } };
  // One answer: a second unit would ask the judge again and find no response left.
  const rows = await replayJudge({ cases: [{ case: 'a', kind: 'acme.note', checks: ['helpful'], content: 'a', units: ['keep'] }], arms: [{ label: 'base', adapters: [two] }], repeat: 1, concurrency: 1, models: judgeModels([false]), judgeModel });
  assert.deepEqual(rows.rows.map((r) => [r.check, r.met]), [['helpful', 0]]);
  assert.deepEqual(rows.rows[0].notes, ['keep: because']);
});

test('a check the judge model could not answer prints null, not a failed check', async () => {
  const faux = fauxProvider({ provider: 'judge', models: [{ id: 'j' }] });
  faux.setResponses([fauxAssistantMessage('no json here')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const rows = await replayJudge({ cases: [cases[0]], arms: [arms[0]], repeat: 1, concurrency: 1, models, judgeModel });
  assert.match(table(rows, ['base']), /\| a \| helpful \| null \| 0\/0 \|/);
});

test('concurrency 2 gives the same table as concurrency 1', async () => {
  const all = [true, true, true, true, true, true, true, true];
  const run = async (concurrency: number) => table(await replayJudge({ cases, arms, repeat: 2, concurrency, models: judgeModels(all), judgeModel }), ['base', 'cand']).split('\n').slice(0, 4);
  assert.deepEqual(await run(2), await run(1));
});

test('a run that faults fails loudly, and so does a judge the deployment lacks', async () => {
  const boom: Arm = { label: 'boom', adapters: [{ kind: 'acme.note', judge: { evidence: async () => undefined, rubric: { name: 'x', version: 1, threshold: 0.5, criteria: [{ id: 'c', checks: [{ id: 'helpful', label: 'L helpful', weight: 'gate', code: () => { throw new Error('bad rubric code'); } }] }] } } }] };
  await assert.rejects(replayJudge({ cases, arms: [boom], repeat: 1, concurrency: 1, models: judgeModels([]), judgeModel }), /bad rubric code/);
  await assert.rejects(replayJudge({ cases, arms, repeat: 1, concurrency: 1, models: judgeModels([]), judgeModel: { provider: 'judge', modelId: 'nope' } }), /judge judge\/nope is not configured/);
});

test('an arm ref unpacks packages/ whether that commit kept them at the root or under v1/', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'quack-arm-ref-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const env = { ...process.env, GIT_AUTHOR_NAME: 't', GIT_AUTHOR_EMAIL: 't@t', GIT_COMMITTER_NAME: 't', GIT_COMMITTER_EMAIL: 't@t', GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' };
  const git = (...args: string[]) => execFileSync('git', args, { cwd: dir, env, encoding: 'utf8' });
  const files = (ref: string) => execFileSync('tar', ['-t'], { input: packagesTar(dir, ref), encoding: 'utf8' }).split('\n').filter((f) => f.endsWith('.ts'));
  git('init', '-q');
  mkdirSync(join(dir, 'v1/packages/x'), { recursive: true });
  writeFileSync(join(dir, 'v1/packages/x/a.ts'), 'export const a = 1;\n');
  git('add', '.');
  git('commit', '-qm', 'nested');
  git('mv', 'v1/packages', 'packages');
  git('commit', '-qm', 'root');
  assert.deepEqual(files('HEAD~1'), ['packages/x/a.ts']);
  assert.deepEqual(files('HEAD'), ['packages/x/a.ts']);
});
