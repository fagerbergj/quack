import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, createProvider, envApiKeyAuth, fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { typesafeSystemOneApi } from '@earendil-works/pi-ai/api/typesafe-system-one.lazy';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Artifacts, artifactsExtension, type ArtifactKind } from '@quack/sdk';
import { openSession } from '@quack/session';
import { feedback, judge, progressOf, score, settled, unitsFor, type CheckResult, type Evidence, type Plan, type Rubric } from '../src/index.ts';

const dirs: string[] = [];
const servers: Array<() => void> = [];
after(async () => {
  servers.forEach((close) => close());
  await Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true })));
});

// A stand-in for Clef speaking its real wire format; probability decides per question text and unit, as Clef never sees a check id.
const clefCalls: Array<{ unit: string; questions: string[] }> = [];
async function fakeClef(probability: (question: string, unit: string) => number) {
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    const { state, questions } = JSON.parse(body);
    clefCalls.push({ unit: state.unit, questions: Object.keys(questions) });
    const answers = Object.fromEntries(Object.entries(questions as Record<string, { instructions: string }>).map(([q, { instructions }]) => [q, { type: 'noul', noul: probability(instructions, state.unit) }]));
    res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ answers, usage: { input_tokens: 100, output_tokens: 0 } }));
  }).listen(0);
  await new Promise((r) => server.once('listening', r));
  servers.push(() => server.close());
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1`;
}

const review: ArtifactKind = { kind: 'github.review', path: 'artifacts/review.md' };

// Findings are lines starting "- blocking:" or "- nit:"; each becomes a unit with only its own text.
async function evidence(_api: unknown, sealed: { content: string }): Promise<Evidence> {
  const findings = sealed.content.split('\n').filter((l) => /^- (blocking|nit):/.test(l))
    .map((line, i) => ({ id: `f${i + 1}`, blocking: line.startsWith('- blocking:'), state: { unit: `f${i + 1}`, finding: line } }));
  return { review: [{ id: 'review', whole: true, state: { unit: 'review', review: sealed.content } }], finding: findings, blocking: findings.filter((f) => f.blocking) };
}

const hasVerdict = (a: string) => /## Verdict/.test(a);
const rubric: Rubric = {
  name: 'reviewer',
  version: 1,
  threshold: 0.8,
  criteria: [
    { id: 'structured_verdict', checks: [
      { id: 'verdict_present', label: 'L verdict_present', weight: 'gate', code: (a) => ({ met: hasVerdict(a) ? 1 : 0, detail: hasVerdict(a) ? undefined : 'no ## Verdict section' }) },
      { id: 'takeaway_actionable', label: 'L takeaway_actionable', weight: 2, scope: 'review', question: 'Is the takeaway one sentence a reader can act on?' },
    ] },
    { id: 'constructive_actionable', checks: [{ id: 'says_why', label: 'L says_why', weight: 3, scope: 'finding', question: 'Does this finding say why it matters?' }] },
    { id: 'severity_grounded', checks: [{ id: 'preference_only', label: 'L preference_only', weight: -3, scope: 'blocking', question: 'Does this blocking finding rest only on preference?' }] },
  ],
};

async function run(baseUrl: string | undefined, maxInputTokens: number, responses: FauxResponseStep[]) {
  const dir = await mkdtemp(join(tmpdir(), 'quack-judge-'));
  dirs.push(dir);
  const faux = fauxProvider({ models: [{ id: 'worker' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  if (baseUrl) {
    const clefModel = { id: 'clef', name: 'clef', type: 'classifier', api: 'typesafe-system-one', provider: 'clef', baseUrl, input: ['text'], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 8192 };
    models.setProvider(createProvider({ id: 'clef', baseUrl, auth: { apiKey: envApiKeyAuth('clef', ['QUACK_CLEF_KEY']) }, models: [clefModel] as never, classifiers: { 'typesafe-system-one': typesafeSystemOneApi() } }));
  }
  const m = faux.getModel();
  const classifier = baseUrl ? { provider: 'clef', modelId: 'clef', maxInputTokens } : undefined;
  const extensions = [CodingTools, artifactsExtension([review]), judge({ kind: review, rubric, evidence: evidence as never, maxRevisions: 2, classifier, toolName: 'submit_review' })];
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
  const done = await session.submit('Review the PR.', 'review-1', true);
  const doc = await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT);
  await session.close();
  return { done, versions: doc!.kinds['github.review'].versions.map((v) => v.verdict as any) };
}

const tool = (name: string, args: Record<string, string>) => fauxAssistantMessage(fauxToolCall(name, args), { stopReason: 'toolUse' });
const yes = fauxAssistantMessage('{"choice": "yes", "confidence": 0.9, "reason": "it explains the risk"}');

test('Clef answers confident checks, an uncertain one escalates, and a failed gate sends the version back', async () => {
  process.env.QUACK_CLEF_KEY = 'none';
  clefCalls.length = 0;
  // Clef is unsure whether f2 says why (0.5); everything else it answers confidently, preference_only at exactly the 0.2 edge.
  const baseUrl = await fakeClef((q, unit) => (q.includes('say why') && unit === 'f2' ? 0.5 : q.includes('only on preference') ? 0.2 : 0.95));
  const good = '## Takeaway\nFix the off-by-one.\n## Verdict\nrequest_changes\n## Findings\n- blocking: src/x.ts:12 off by one\n- nit: src/y.ts:3 rename\n';
  const { done, versions } = await run(baseUrl, 8192, [
    tool('write', { path: 'artifacts/review.md', content: good.replace('## Verdict\nrequest_changes\n', '') }),
    tool('submit_review', {}),
    yes,
    fauxAssistantMessage('Done.'),
    tool('write', { path: 'artifacts/review.md', content: good }),
    tool('submit_review', {}),
    yes,
    fauxAssistantMessage('Submitted v2.'),
  ]);
  assert.equal(done.status === 'done' && done.answer, 'Submitted v2.');
  assert.deepEqual(versions[0].gatesFailed, ['structured_verdict.verdict_present']);
  assert.equal(versions[0].passed, false);

  const v2 = versions[1];
  assert.equal(v2.passed, true);
  assert.equal(v2.score, 1);
  const says = v2.checks.find((c: any) => c.id === 'says_why');
  assert.deepEqual(says.resolvers, { clef: 1, 'agent-model': 1 }, 'f1 by Clef, f2 escalated');
  assert.equal(says.met, 1);
  const preference = v2.checks.find((c: any) => c.id === 'preference_only');
  assert.equal(preference.units, 1, 'only the blocking finding');
  assert.deepEqual(preference.resolvers, { clef: 1 }, 'p = 0.2 is taken as given, not escalated');
  assert.ok(clefCalls.some((c) => c.unit === 'f1' && c.questions.length === 2), 'one request per unit, all its checks together');
});

test('a unit over the decision model cap skips it and every check escalates', async () => {
  clefCalls.length = 0;
  const baseUrl = await fakeClef(() => 0.95);
  const content = '## Takeaway\nLooks good.\n## Verdict\napprove\n## Findings\n- nit: src/y.ts:3 rename\n';
  const { versions } = await run(baseUrl, 10, [tool('write', { path: 'artifacts/review.md', content }), tool('submit_review', {}), yes, yes, fauxAssistantMessage('Done.')]);
  assert.equal(clefCalls.length, 0);
  assert.ok(versions[0].checks.filter((c: any) => c.resolvers['agent-model']).length >= 2);
  assert.equal(versions[0].passed, true);
});

const busy = fauxAssistantMessage('', { stopReason: 'error', errorMessage: '429 concurrency_limit' });

test('a judge model that errors leaves checks undecided: no verdict is attached', async () => {
  clefCalls.length = 0;
  const baseUrl = await fakeClef(() => 0.95);
  const content = '## Takeaway\nLooks good.\n## Verdict\napprove\n## Findings\n- nit: src/y.ts:3 rename\n';
  // Clef is over its cap, so every model check goes to the agent model: one answers, the other errors.
  const { versions } = await run(baseUrl, 10, [tool('write', { path: 'artifacts/review.md', content }), tool('submit_review', {}), yes, busy, fauxAssistantMessage('Done.')]);
  assert.equal(versions[0]?.passed, undefined, 'an outage is not a verdict');
});

test('an undecided check is unscored, never unmet', () => {
  const r = { criterion: 'c', id: 'k', label: 'k', weight: 2, met: null, units: 1, resolvers: { none: 1 }, notes: [], undecided: 1 };
  const ok = { criterion: 'c', id: 'j', label: 'j', weight: 2, met: 1, units: 1, resolvers: { code: 1 }, notes: [] };
  assert.deepEqual(score({ name: 'x', version: 1, threshold: 0.8, criteria: [] }, [r, ok]), { score: 1, gatesFailed: [], passed: true });
  assert.deepEqual(feedback([r]), []);
});

test('a whole-artifact unit adds no prefix to its notes, another unit keeps its id', async () => {
  const no = fauxAssistantMessage('{"choice": "no", "confidence": 0.9, "reason": "too long"}');
  const content = '## Takeaway\nLooks good.\n## Verdict\napprove\n## Findings\n- nit: src/y.ts:3 rename\n';
  const { versions } = await run(undefined, 10, [tool('write', { path: 'artifacts/review.md', content }), tool('submit_review', {}), no, no, fauxAssistantMessage('Done.')]);
  const notes = (id: string) => versions[0].checks.find((c: any) => c.id === id).notes;
  assert.deepEqual(notes('takeaway_actionable'), ['too long']);
  assert.deepEqual(notes('says_why'), ['f1: too long']);
});

test('a pitfall keeps the reason where it is present, a met check keeps none', async () => {
  const yes = (reason: string) => fauxAssistantMessage(`{"choice": "yes", "confidence": 0.9, "reason": "${reason}"}`);
  const content = '## Takeaway\nFix the loop.\n## Verdict\nrequest changes\n## Findings\n- blocking: src/y.ts:3 I prefer tabs\n';
  const { versions } = await run(undefined, 10, [tool('write', { path: 'artifacts/review.md', content }), tool('submit_review', {}), yes('one sentence'), yes('says why'), yes('only taste'), fauxAssistantMessage('Done.')]);
  const notes = (id: string) => versions[0].checks.find((c: any) => c.id === id).notes;
  assert.deepEqual([notes('takeaway_actionable'), notes('says_why'), notes('preference_only')], [[], [], ['f1: only taste']]);
});

// A legacy plan (no aggregate) whose units are already answered, as Clef answers them: confident and without a reason.
const answeredPlan = (id: string, weight: number, answers: Array<boolean | null>, whole = false): Plan => ({
  key: `c.${id}`, result: { criterion: 'c', id, label: id, weight, met: 0, units: answers.length, resolvers: {}, notes: [] }, ask: { id: `c.${id}`, question: { type: 'bool', instructions: '?' } },
  pitfall: weight < 0, left: 0, units: answers.map((_, i) => ({ id: `f${i + 1}`, state: {}, ...(whole ? { whole: true as const } : {}) })),
  answers: new Map(answers.map((met, i) => [`f${i + 1}`, { met, resolver: 'clef' }])),
});

test('a failing item Clef answered without a reason is still named in the notes', () => {
  const [why, pref, takeaway] = settled([answeredPlan('says_why', 3, [true, false]), answeredPlan('preference_only', -3, [true, false]), answeredPlan('takeaway', 2, [false], true)]);
  assert.deepEqual([why.met, why.notes], [0.5, ['f2: flagged by clef (no reason given)']]);
  assert.deepEqual([pref.met, pref.notes], [0.5, ['f1: flagged by clef (no reason given)']], 'a pitfall names where it is present');
  assert.deepEqual(takeaway.notes, ['flagged by clef (no reason given)']);
  assert.deepEqual(feedback([why]), ['missed c.says_why (1 of 2): f2: flagged by clef (no reason given)']);
});

test('a share over every unit met is exactly 1: units are counted, not summed as fractions', () => {
  for (const n of [3, 6, 7, 10, 23]) assert.equal(settled([answeredPlan('k', 3, Array(n).fill(true))])[0].met, 1, `${n} units`);
});

test('a check without a scope is an error, not a default scope', () => {
  assert.throws(() => unitsFor(undefined, { review: [] }), /name its evidence scope/);
  assert.deepEqual(unitsFor('review', { review: [] }), []);
});

test('progress withholds a check until every unit is judged and decided, and passes code results through', () => {
  const r = (id: string, units: number): CheckResult => ({ criterion: 'c', id, label: id, weight: 2, met: units ? 0 : null, units, resolvers: {}, notes: [] });
  const plan = (id: string, answers: Array<boolean | null>, left: number): Plan => ({
    key: `c.${id}`, result: r(id, answers.length), ask: { id: `c.${id}`, question: { type: 'bool', instructions: '?' } }, pitfall: false, left,
    units: answers.map((_, i) => ({ id: `u${i}`, state: {} })), answers: new Map(answers.map((met, i) => [`u${i}`, { met, resolver: 'clef' }])),
  });
  const code = { ...r('verdict_present', 1), met: 1 };
  const rubric2: Rubric = { name: 'x', version: 1, threshold: 0.8, criteria: [{ id: 'c', checks: ['verdict_present', 'a', 'b', 'u', 'n'].map((id) => ({ id, label: id, weight: 2 })) }] };
  const plans = [plan('a', [true, false], 1), plan('b', [true, true], 0), plan('u', [true, null], 0), plan('n', [], 0)];
  const mets = () => progressOf(rubric2, [code, ...settled(plans)]).checks.map((c) => c.met);
  assert.deepEqual(mets(), [1, undefined, 1, undefined, null], 'a partial and an undecided check are still checking; one with no unit is null');
  plans[0].left = 0;
  assert.deepEqual(mets(), [1, 0.5, 1, undefined, null], 'once every unit is judged the share shows');
});
