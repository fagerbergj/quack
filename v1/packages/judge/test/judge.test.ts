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
import { feedback, judge, progressOf, score, settled, unitsFor, type CheckResult, type Evidence, type Rubric } from '../src/index.ts';

const dirs: string[] = [];
const servers: Array<() => void> = [];
after(async () => {
  servers.forEach((close) => close());
  await Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true })));
});

// A stand-in for Clef speaking its real wire format; probability decides per question and unit.
const clefCalls: Array<{ unit: string; questions: string[] }> = [];
const clefLoad = { now: 0, peak: 0 };
async function fakeClef(probability: (question: string, unit: string) => number, holdMs = 0) {
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    const { state, questions } = JSON.parse(body);
    clefCalls.push({ unit: state.unit, questions: Object.keys(questions) });
    clefLoad.peak = Math.max(clefLoad.peak, ++clefLoad.now);
    await new Promise((r) => setTimeout(r, holdMs));
    clefLoad.now--;
    const answers = Object.fromEntries(Object.keys(questions).map((q) => [q, { type: 'noul', noul: probability(q, state.unit) }]));
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

async function run(baseUrl: string | undefined, maxInputTokens: number, responses: FauxResponseStep[], concurrency?: number) {
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
  const classifier = baseUrl ? { provider: 'clef', modelId: 'clef', maxInputTokens, concurrency } : undefined;
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
  const baseUrl = await fakeClef((q, unit) => (q.endsWith('says_why') && unit === 'f2' ? 0.5 : q.endsWith('preference_only') ? 0.2 : 0.95));
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

test('with concurrency set, units are classified at once and the verdict matches one at a time', async () => {
  const content = '## Takeaway\nFix them.\n## Verdict\nrequest_changes\n## Findings\n- blocking: src/a.ts:1 off by one\n- nit: src/b.ts:2 rename\n- nit: src/c.ts:3 typo\n';
  const once = async (concurrency?: number) => {
    Object.assign(clefLoad, { now: 0, peak: 0 });
    const baseUrl = await fakeClef((q) => (q.endsWith('preference_only') ? 0.05 : 0.95), 30);
    const { versions } = await run(baseUrl, 8192, [tool('write', { path: 'artifacts/review.md', content }), tool('submit_review', {}), fauxAssistantMessage('Done.')], concurrency);
    return { peak: clefLoad.peak, verdict: versions[0] };
  };
  const serial = await once();
  const parallel = await once(4);
  const capped = await once(2);
  assert.equal(serial.peak, 1);
  assert.equal(parallel.peak, 4, 'the review unit and the three findings go out together');
  assert.equal(capped.peak, 2, 'never more than `concurrency` at once');
  assert.deepEqual(parallel.verdict, serial.verdict);
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

test('a check without a scope is an error, not a default scope', () => {
  assert.throws(() => unitsFor(undefined, { review: [] }), /name its evidence scope/);
  assert.deepEqual(unitsFor('review', { review: [] }), []);
});

test('progress withholds a check until every unit is judged and decided, and passes code results through', () => {
  const r = (id: string, over: Partial<CheckResult>): CheckResult => ({ criterion: 'c', id, label: id, weight: 2, met: 0, units: 2, resolvers: {}, notes: [], ...over });
  const code = r('verdict_present', { met: 1, units: 1 });
  const rubric2: Rubric = { name: 'x', version: 1, threshold: 0.8, criteria: [{ id: 'c', checks: [{ id: 'verdict_present', label: 'v', weight: 'gate' }, { id: 'a', label: 'a', weight: 2 }, { id: 'b', label: 'b', weight: 2 }, { id: 'u', label: 'u', weight: 2 }] }] };
  const results = new Map([['c.a', r('a', { met: 0.5 })], ['c.b', r('b', { met: 1 })], ['c.u', r('u', { met: 0.5, undecided: 1 })]]);
  const left = new Map([['c.a', 1], ['c.b', 0], ['c.u', 0]]);
  const mets = () => progressOf(rubric2, [code, ...settled(results, left)]).checks.map((c) => c.met);
  assert.deepEqual(mets(), [1, undefined, 1, undefined], 'a partial and an undecided check are still checking');
  left.set('c.a', 0);
  assert.deepEqual(mets(), [1, 0.5, 1, undefined], 'once every unit is judged the weighted fraction shows');
  results.get('c.u')!.met = null;
  results.get('c.u')!.undecided = undefined;
  assert.deepEqual(mets(), [1, 0.5, 1, null], 'a check with no applicable unit is decided as null');
});
