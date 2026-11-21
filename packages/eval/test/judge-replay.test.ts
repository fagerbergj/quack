import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { cpSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { test, type TestContext } from 'node:test';
import { createModels, createProvider, envApiKeyAuth, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { typesafeSystemOneApi } from '@earendil-works/pi-ai/api/typesafe-system-one.lazy';
import type { Declaration, Rubric } from '@quack/judge';
import type { EvalAdapter } from '@quack/sdk';
import { armAt, loadArm, packagesTar, replayJudge, table, type Arm, type Case, type Row } from '../src/index.ts';

const rubric: Rubric = {
  name: 'acme', version: 1, threshold: 0.5,
  criteria: [{ id: 'c', checks: [{ id: 'helpful', label: 'L helpful', weight: 2 }, { id: 'other', label: 'L other', weight: 1 }] }],
};
const declaration = (question: string): Declaration => ({
  scopes: { note: { split: { name: 'jsonItems', from: 'note' } }, gap: { split: { name: 'jsonItems', from: 'gap' } } },
  checks: {
    helpful: { scope: 'note', aggregate: { name: 'share' }, question: { type: 'bool', instructions: question } },
    other: { scope: 'gap', aggregate: { name: 'share' }, question: { type: 'bool', instructions: 'Unwatched?' } },
  },
});
const adapter = (question: string): EvalAdapter => ({ kind: 'acme.note', judge: { rubric, declaration: declaration(question), inputs: async () => ({ note: '{}', gap: '[]' }) } });
const arms: Arm[] = [{ label: 'base', adapters: [adapter('Is it helpful?')] }, { label: 'cand', adapters: [adapter('Does it help a reader act?')] }];
const cases: Case[] = ['a', 'b'].map((c) => ({ case: c, kind: 'acme.note', checks: ['helpful'], content: c, inputs: { note: JSON.stringify({ [c]: { text: c } }), gap: '[]' } }));
const answer = (yes: boolean) => fauxAssistantMessage(JSON.stringify({ choice: yes ? 'yes' : 'no', confidence: 0.9, reason: 'because' }));
const judgeModel = { provider: 'judge', modelId: 'j' };

function judgeModels(answers: boolean[]) {
  const faux = fauxProvider({ provider: 'judge', models: [{ id: 'j' }] });
  faux.setResponses(answers.map(answer));
  const models = createModels();
  models.setProvider(faux.provider);
  return models;
}

/** A judge model that answers by what it is asked, so the arm order does not change the answers; `asked` is each request's arm and case. */
function judgeBy(answer: (sent: string) => boolean) {
  const asked: string[] = [];
  const faux = fauxProvider({ provider: 'judge', models: [{ id: 'j' }] });
  const reply = (context: { messages: Array<{ content: unknown }> }) => {
    const sent = String(context.messages.at(-1)?.content);
    asked.push(`${sent.includes('reader act') ? 'cand' : 'base'} ${/"text": "(\w)"/.exec(sent)?.[1]}`);
    return answer(sent) ? fauxAssistantMessage(JSON.stringify({ choice: 'yes', confidence: 0.9, reason: 'because' })) : fauxAssistantMessage(JSON.stringify({ choice: 'no', confidence: 0.9, reason: 'because' }));
  };
  faux.setResponses(Array.from({ length: 64 }, () => reply) as never);
  const models = createModels();
  models.setProvider(faux.provider);
  return { models, asked };
}
const baseSaysNoToB = (sent: string) => sent.includes('reader act') || !sent.includes('"text": "b"');

test('two cases, two arms and repeat 2 give the met values of each repeat for the watched check, then counts before seconds per arm', async () => {
  const { models } = judgeBy(baseSaysNoToB);
  const rows = await replayJudge({ cases, arms, repeat: 2, concurrency: 1, models, judgeModel, seed: 7 });
  const out = table(rows, ['base', 'cand']).split('\n');
  assert.deepEqual(out.slice(0, 4), [
    '| case | check | met base | met cand | clef/esc base | clef/esc cand |',
    '| --- | --- | --- | --- | --- | --- |',
    '| a | helpful | 1 1 | 1 1 | 0/2 | 0/2 |',
    '| b | helpful | 0 0 | 1 1 | 0/2 | 0/2 |',
  ]);
  assert.equal(out[5], 'per arm (requests and settled items, then judge seconds per case):');
  for (const [i, arm] of [[6, 'base'], [7, 'cand']] as const) {
    assert.match(out[i], new RegExp(`^- ${arm}: requests 4 \\(j 4\\); items decision model 0, escalated 4, reused 0; judge seconds per case median \\d+\\.\\d p90 \\d+\\.\\d, by place 1: median .*; 2: median `));
  }
  assert.equal(out[8], 'arm order seed 7');
  assert.deepEqual(out.slice(10), ['disagreements (case check: met per repeat by arm, then the notes each arm gave):', '- b helpful: base 0 0; cand 1 1', '  - base: note b: because']);
});

test('arms are interleaved case by case in a seeded order, so each arm takes each place and a seed replays the same order', async () => {
  const run = async (seed: number) => {
    const { models, asked } = judgeBy(() => true);
    const { rows } = await replayJudge({ cases, arms, repeat: 2, concurrency: 1, models, judgeModel, seed });
    return { asked, rows };
  };
  const { asked, rows } = await run(3);
  // Repeat by repeat and case by case: each pair of requests is one case under both arms, in that case's order.
  assert.deepEqual(asked.map((a) => a.split(' ')[1]), ['a', 'a', 'b', 'b', 'a', 'a', 'b', 'b']);
  for (let i = 0; i < 8; i += 2) assert.notEqual(asked[i].split(' ')[0], asked[i + 1].split(' ')[0]);
  assert.deepEqual(rows.map((r) => `${r.rep} ${r.case} ${r.arm} ${r.pos}`), asked.map((a, i) => `${i < 4 ? 1 : 2} ${a.split(' ')[1]} ${a.split(' ')[0]} ${(i % 2) + 1}`), 'each row says the place its arm judged in');
  assert.deepEqual((await run(3)).asked, asked, 'the same seed, the same order');
  const places = new Set<string>();
  for (let seed = 0; seed < 8; seed++) for (const r of (await run(seed)).rows) places.add(`${r.arm} ${r.pos}`);
  assert.deepEqual([...places].sort(), ['base 1', 'base 2', 'cand 1', 'cand 2'], 'over seeds, each arm takes each place');
});

test('each job\'s rows are handed over as it finishes, so a run that fails later keeps them', async () => {
  const written: Row[][] = [];
  const later: EvalAdapter = { kind: 'acme.note', judge: { declaration: { scopes: {}, checks: {} }, inputs: async () => ({}), rubric: { name: 'x', version: 1, threshold: 0.5, criteria: [{ id: 'c', checks: [{ id: 'helpful', label: 'L', weight: 'gate', code: (a: string) => { if (a === 'b') throw new Error('stopped'); return { met: 1 }; } }] }] } } };
  await assert.rejects(replayJudge({ cases, arms: [{ label: 'base', adapters: [later] }], repeat: 1, concurrency: 1, models: judgeModels([]), judgeModel, seed: 1, onRows: (rows) => written.push(rows) }), /stopped/);
  assert.deepEqual(written.map((rows) => rows.map((r) => [r.case, r.check, r.met, r.requests])), [[['a', 'helpful', 1, {}]]]);
});

// A stand-in for Clef on its wire format, so an arm's decision model is really asked.
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

test('arms that differ only in decision model: the decision-model arm answers from Clef, the other escalates, and the disagreement is listed', async (t) => {
  const models = judgeModels([true]);
  models.setProvider(await fakeClef(t, 0.05));
  const same = adapter('Is it helpful?');
  const result = await replayJudge({ cases: [cases[0]], arms: [{ label: 'base', adapters: [same] }, { label: 'clef', adapters: [same], decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } }], repeat: 1, concurrency: 1, models, judgeModel });
  const out = table(result, ['base', 'clef']).split('\n');
  assert.equal(out[2], '| a | helpful | 1 | 0 | 0/1 | 1/0 |');
  assert.match(out[5], /^- base: requests 1 \(j 1\); items decision model 0, escalated 1, reused 0;/);
  assert.match(out[6], /^- clef: requests 1 \(clef 1\); items decision model 1, escalated 0, reused 0;/, 'a decision-model request counts under its model');
  assert.equal(out[10], '- a helpful: base 1; clef 0');
});

test('an adapter that cannot rebuild a case declines it with its reason, and the other cases are still judged', async () => {
  const picky: EvalAdapter = { kind: 'acme.note', judge: { rubric, declaration: declaration('Is it helpful?'), inputs: async (_s, content) => { throw new Error(`the recording of ${content} keeps no facts`); } } };
  const bare = cases.map(({ inputs: _i, ...c }) => c);
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

test('a case naming units judges only those items of a declared rubric, by item id or unit id', async () => {
  const declared: Rubric = { name: 'acme', version: 1, threshold: 0.5, criteria: [{ id: 'c', checks: [{ id: 'helpful', label: 'L helpful', weight: 2 }] }] };
  const decl = { scopes: { note: { split: { name: 'jsonItems', from: 'note' } } }, checks: { helpful: { scope: 'note', aggregate: { name: 'share' as const }, question: { type: 'bool' as const, instructions: 'Is it helpful?' } } } };
  const two: EvalAdapter = { kind: 'acme.note', judge: { rubric: declared, declaration: decl, inputs: async () => ({ note: JSON.stringify({ keep: 'a', drop: 'b' }) }) } };
  for (const units of [['keep'], ['note keep']]) {
    const rows = await replayJudge({ cases: [{ case: 'a', kind: 'acme.note', content: 'a', units }], arms: [{ label: 'base', adapters: [two] }], repeat: 1, concurrency: 1, models: judgeModels([false]), judgeModel });
    assert.deepEqual(rows.rows.map((r) => [r.check, r.met, r.notes]), [['helpful', 0, ['note keep: because']]], units[0]);
  }
});

test('a check the judge model could not answer prints null, not a failed check', async () => {
  const faux = fauxProvider({ provider: 'judge', models: [{ id: 'j' }] });
  faux.setResponses([fauxAssistantMessage('no json here')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const rows = await replayJudge({ cases: [cases[0]], arms: [arms[0]], repeat: 1, concurrency: 1, models, judgeModel });
  assert.match(table(rows, ['base']), /\| a \| helpful \| null \| 0\/0 \|/);
});

test('a reuse arm judges one session\'s cases in version order against a store of their own per repeat, interleaved with the other arms; an arm without it asks every case', async () => {
  // v2 listed first, same inputs as v1; the reuse arm must judge v1 first and reuse its answer for v2.
  const drafts: Case[] = [2, 1].map((version) => ({ case: `s v${version}`, kind: 'acme.note', session: '/nonexistent/s', version, checks: ['helpful'], content: `v${version}`, inputs: { note: JSON.stringify({ n: { text: 'same' } }), gap: '[]' } }));
  // The reuse arm's first ask is met, its second (the next repeat's fresh store) is not; the other arm's answers are not checked.
  let candAsks = 0;
  const { models } = judgeBy((sent) => !sent.includes('reader act') || candAsks++ === 0);
  const { rows } = await replayJudge({ cases: drafts, arms: [{ label: 'cand', adapters: [adapter('Does it help a reader act?')], reuse: true }, { label: 'base', adapters: [adapter('Is it helpful?')] }], repeat: 2, concurrency: 1, models, judgeModel, seed: 5 });
  const of = (arm: string, c: string) => rows.filter((r) => r.arm === arm && r.case === c).map((r) => [r.rep, r.met, r.esc, r.reused, r.notes]);
  assert.deepEqual(of('cand', 's v1'), [[1, 1, 1, 0, []], [2, 0, 1, 0, ['note n: because']]]);
  assert.deepEqual(of('cand', 's v2'), [[1, 1, 0, 1, []], [2, 0, 0, 1, ['note n: because (judged on v1)']]], 'v2 reuses the answer v1 got in the same repeat');
  assert.deepEqual(of('base', 's v2').map((r) => r[2]).concat(of('base', 's v1').map((r) => r[2])), [1, 1, 1, 1], 'without reuse every case is asked');
  const cand = rows.filter((r) => r.arm === 'cand');
  assert.deepEqual(cand.map((r) => `${r.rep} ${r.case}`), ['1 s v1', '1 s v2', '2 s v1', '2 s v2'], 'each repeat\'s drafts run back to back, in version order');
  assert.deepEqual(new Set(cand.filter((r) => r.rep === 1).map((r) => r.pos)).size, 1, 'the batch takes one place, its first case\'s');
});

test('concurrency 2 gives the same table as concurrency 1', async () => {
  const all = [true, true, true, true, true, true, true, true];
  const run = async (concurrency: number) => table(await replayJudge({ cases, arms, repeat: 2, concurrency, models: judgeModels(all), judgeModel }), ['base', 'cand']).split('\n').slice(0, 4);
  assert.deepEqual(await run(2), await run(1));
});

test('a run that faults fails loudly, and so does a judge the deployment lacks', async () => {
  const boom: Arm = { label: 'boom', adapters: [{ kind: 'acme.note', judge: { declaration: { scopes: {}, checks: {} }, inputs: async () => ({}), rubric: { name: 'x', version: 1, threshold: 0.5, criteria: [{ id: 'c', checks: [{ id: 'helpful', label: 'L helpful', weight: 'gate', code: () => { throw new Error('bad rubric code'); } }] }] } } }] };
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

/** This tree's packages copied to a temp dir, each patch a [file, from, to] replacing every occurrence, loaded as an arm. */
async function otherTree(t: TestContext, patches: [string, string, string][]) {
  const dir = mkdtempSync(join(tmpdir(), 'quack-arm-engine-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  cpSync(fileURLToPath(new URL('../../', import.meta.url)), join(dir, 'packages'), { recursive: true, filter: (src) => !src.includes('node_modules') });
  for (const [file, from, to] of patches) {
    const s = readFileSync(join(dir, 'packages', file), 'utf8');
    assert.ok(s.includes(from), `${file} no longer has ${from}`);
    writeFileSync(join(dir, 'packages', file), s.replaceAll(from, to));
  }
  return loadArm(dir);
}

test('an arm from another tree judges with that tree\'s judge and SDK, so each arm\'s requests come from its own engine', async (t) => {
  // The other tree's judge marks the question and its SDK sets a temperature, both on the judge model's request.
  const there = await otherTree(t, [
    ['judge/src/resolve.ts', ': q.instructions;', ': `${q.instructions} (other judge)`;'],
    ['sdk/src/decide.ts', '{ signal: ctx.abortSignal, fetch })), ctx.abortSignal, seams)', '{ signal: ctx.abortSignal, fetch, temperature: 0.25 })), ctx.abortSignal, seams)'],
  ]);
  const seen: [string, boolean, number | undefined][] = [];
  const faux = fauxProvider({ provider: 'judge', models: [{ id: 'j' }] });
  const record = (context: { messages: unknown[] }, options?: { temperature?: number }) => {
    const sent = JSON.stringify(context.messages);
    seen.push([sent.includes('arm here') ? 'here' : 'there', sent.includes('(other judge)'), options?.temperature]);
    return answer(true);
  };
  faux.setResponses([record, record]);
  const models = createModels();
  models.setProvider(faux.provider);
  const arms: Arm[] = [{ label: 'here', adapters: [adapter('Is it helpful, arm here?')] }, { label: 'there', adapters: [adapter('Is it helpful, arm there?')], rejudge: there.rejudge }];
  await replayJudge({ cases: [cases[0]], arms, repeat: 1, concurrency: 1, models, judgeModel });
  assert.deepEqual(seen.sort(), [['here', false, undefined], ['there', true, 0.25]]);
  assert.ok(there.adapters.length, 'the other tree\'s extensions load beside its engine');
});

test('a ref arm keeps its tree, so its contracts TypeScript lint still resolves @types/node', async () => {
  const before = new Set(readdirSync(tmpdir()));
  await armAt('HEAD');
  const dir = join(tmpdir(), readdirSync(tmpdir()).find((d) => d.startsWith('quack-arm-') && !before.has(d))!);
  const { lintTypeScript } = (await import(pathToFileURL(join(dir, 'packages/contracts/src/index.ts')).href)) as typeof import('@quack/contracts');
  assert.deepEqual(await lintTypeScript('/** Provided here, consumed by callers. */\nexport type Pid = NodeJS.Process["pid"];\n'), []);
});

test('watch skips checks an arm lacks (erroring only if no arm has them) and table filters reported checks while keeping full-round per-arm counts', async () => {
  const baseAdapter = adapter('Is it helpful?');
  const candRubric: Rubric = { name: 'acme', version: 2, threshold: 0.5, criteria: [{ id: 'c', checks: [{ id: 'helpful', label: 'L helpful', weight: 2 }, { id: 'extra', label: 'L extra', weight: 1 }] }] };
  const candDecl = { scopes: { note: { split: { name: 'jsonItems', from: 'note' } } }, checks: { helpful: { scope: 'note', aggregate: { name: 'share' as const }, question: { type: 'bool' as const, instructions: 'Is it helpful?' } }, extra: { scope: 'note', aggregate: { name: 'share' as const }, question: { type: 'bool' as const, instructions: 'Extra?' } } } };
  const candAdapter: EvalAdapter = { kind: 'acme.note', judge: { rubric: candRubric, declaration: candDecl, inputs: async () => ({}) } };
  const pair: Arm[] = [{ label: 'base', adapters: [baseAdapter] }, { label: 'cand', adapters: [candAdapter] }];
  // Watching a check only cand has succeeds across both arms.
  const watched = await replayJudge({ cases: [{ ...cases[0], checks: ['helpful', 'extra'] }], arms: pair, repeat: 1, concurrency: 1, models: judgeModels([true, true, true]), judgeModel });
  assert.deepEqual(watched.rows.map((r) => `${r.arm}:${r.check}`).sort(), ['base:helpful', 'cand:extra', 'cand:helpful']);
  // Full-round judging with table(..., ['extra']) shows only extra in rows while per-arm still counts both cand checks.
  const full = await replayJudge({ cases: [{ ...cases[0], checks: undefined }], arms: pair, repeat: 1, concurrency: 1, models: judgeModels([true, true, true]), judgeModel });
  const md = table(full, ['base', 'cand'], ['extra']);
  assert.match(md, /\| a \| extra \|/);
  assert.doesNotMatch(md, /\| a \| helpful \|/);
  assert.match(md, /- cand: requests 2 \(j 2\); items decision model 0, escalated 2/);
  await assert.rejects(replayJudge({ cases: [{ ...cases[0], checks: ['missing'] }], arms: pair, repeat: 1, concurrency: 1, models: judgeModels([]), judgeModel }), /no check missing/);
});
