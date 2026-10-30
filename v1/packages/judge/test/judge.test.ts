import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Artifacts, artifactsExtension, type ArtifactKind } from '@quack/sdk';
import { openSession } from '@quack/session';
import { judge } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

const review: ArtifactKind = { kind: 'github.review', path: 'artifacts/review.md' };
const criteria = [
  { id: 'cites_lines', question: 'Does every finding cite a file and line?' },
  { id: 'has_verdict', question: 'Does the review end with a clear verdict?' },
];

const tool = (name: string, args: Record<string, string>) => fauxAssistantMessage(fauxToolCall(name, args), { stopReason: 'toolUse' });
const verdict = (choice: 'pass' | 'fail', reason: string) => fauxAssistantMessage(JSON.stringify({ choice, confidence: 0.9, reason }));

async function run(maxRevisions: number, responses: FauxResponseStep[]) {
  const dir = await mkdtemp(join(tmpdir(), 'quack-judge-'));
  dirs.push(dir);
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const extensions = [CodingTools, artifactsExtension([review]), judge({ kind: review, criteria, maxRevisions, toolName: 'submit_review' })];
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
  const done = await session.submit('Review the PR.', 'review-1', true);
  const doc = await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT);
  await session.close();
  return { done, versions: doc!.kinds['github.review'].versions };
}

test('a failing version is sent back for revision, and the run cannot finish until a version passes', async () => {
  const checkRequests: unknown[][] = [];
  const checked = (step: ReturnType<typeof verdict>) => (context: { messages: unknown[] }) => (checkRequests.push(context.messages), step);
  const { done, versions } = await run(1, [
    tool('write', { path: 'artifacts/review.md', content: 'Looks fine.\n\nVerdict: approve' }),
    fauxAssistantMessage('Done.'),
    tool('submit_review', {}),
    checked(verdict('fail', 'no file or line is cited')),
    checked(verdict('pass', 'ends with approve')),
    fauxAssistantMessage('Done.'),
    tool('write', { path: 'artifacts/review.md', content: 'src/x.ts:12 off by one.\n\nVerdict: request changes' }),
    tool('submit_review', {}),
    checked(verdict('pass', 'cites src/x.ts:12')),
    checked(verdict('pass', 'ends with request changes')),
    fauxAssistantMessage('Submitted v2.'),
  ]);

  assert.equal(done.status === 'done' && done.answer, 'Submitted v2.');
  assert.equal(versions.length, 2);
  assert.equal((versions[0].verdict as { passed: boolean }).passed, false);
  assert.deepEqual((versions[1].verdict as { passed: boolean }).passed, true);
  assert.equal((versions[1].verdict as { criteria: { resolver: string }[] }).criteria[0].resolver, 'agent-model');
  // Each check sees only the criterion and the artifact, not the reviewer's conversation.
  for (const messages of checkRequests) {
    assert.equal(messages.length, 1);
    assert.doesNotMatch(JSON.stringify(messages), /Review the PR/);
  }
  assert.match(JSON.stringify(checkRequests[0]), /Looks fine/);
});

test('past the revision limit a failing version may finish, with its failures marked', async () => {
  const { done, versions } = await run(0, [
    tool('write', { path: 'artifacts/review.md', content: 'Looks fine.' }),
    tool('submit_review', {}),
    verdict('fail', 'no file or line is cited'),
    verdict('fail', 'no verdict'),
    fauxAssistantMessage('Delivering v1 with failures marked.'),
  ]);
  assert.equal(done.status === 'done' && done.answer, 'Delivering v1 with failures marked.');
  const v = versions[0].verdict as { passed: boolean; limitReached: boolean; criteria: { passed: boolean }[] };
  assert.equal(v.passed, false);
  assert.equal(v.limitReached, true);
  assert.deepEqual(v.criteria.map((c) => c.passed), [false, false]);
});
