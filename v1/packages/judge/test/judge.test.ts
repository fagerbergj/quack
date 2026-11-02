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
import { judge, type Evidence, type Rubric } from '../src/index.ts';

const dirs: string[] = [];
const servers: Array<() => void> = [];
after(async () => {
  servers.forEach((close) => close());
  await Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true })));
});

// A stand-in for Clef speaking its real wire format; probability decides per question and unit.
const clefCalls: Array<{ unit: string; questions: string[] }> = [];
async function fakeClef(probability: (question: string, unit: string) => number) {
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    const { state, questions } = JSON.parse(body);
    clefCalls.push({ unit: state.unit, questions: Object.keys(questions) });
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
  return { review: [{ id: 'review', state: { unit: 'review', review: sealed.content } }], finding: findings, blocking: findings.filter((f) => f.blocking) };
}

const hasVerdict = (a: string) => /## Verdict/.test(a);
const rubric: Rubric = {
  name: 'reviewer',
  version: 1,
  threshold: 0.8,
  criteria: [
    { id: 'structured_verdict', checks: [
      { id: 'verdict_present', weight: 'gate', code: (a) => ({ met: hasVerdict(a) ? 1 : 0, detail: hasVerdict(a) ? undefined : 'no ## Verdict section' }) },
      { id: 'takeaway_actionable', weight: 2, scope: 'review', question: 'Is the takeaway one sentence a reader can act on?' },
    ] },
    { id: 'constructive_actionable', checks: [{ id: 'says_why', weight: 3, scope: 'finding', question: 'Does this finding say why it matters?' }] },
    { id: 'severity_grounded', checks: [{ id: 'preference_only', weight: -3, scope: 'blocking', question: 'Does this blocking finding rest only on preference?' }] },
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
  const baseUrl = await fakeClef((q, unit) => (q.endsWith('says_why') && unit === 'f2' ? 0.5 : q.endsWith('preference_only') ? 0.2 : 0.95));
  const good = '## Takeaway\nFix the off-by-one.\n## Verdict\nrequest_changes\n## Findings\n- blocking: src/x.ts:12 off by one\n- nit: src/y.ts:3 rename\n';
  const { done, versions } = await run(baseUrl, 8192, [
    tool('write', { path: 'artifacts/review.md', content: good.replace('## Verdict\nrequest_changes\n', '') }),
    tool('submit_review', {}),
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
