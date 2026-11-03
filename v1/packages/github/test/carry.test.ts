import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { defineExtension } from '@earendil-works/pi-durable';
import { openSession } from '@quack/session';
import { Artifacts } from '@quack/sdk';
import { checkoutTool, PullRequestDoc } from '../src/index.ts';
import { cleanup, fakeService, sh, tmp } from './fixtures.ts';

after(cleanup);

const REVIEW = 'https://github.com/acme/widgets/pull/7#review-1';

// A PR whose old head was approved, then rebased onto a base that moved on: `change` is what the new head sets app.ts's constant to.
async function rebased(change: string) {
  const root = await tmp();
  const work = await tmp();
  const write = async (files: Record<string, string>) => {
    for (const [path, text] of Object.entries(files)) {
      await mkdir(dirname(join(work, path)), { recursive: true });
      await writeFile(join(work, path), text);
    }
  };
  sh(work, 'init', '-q', '-b', 'main');
  await write({ 'app.ts': 'export const a = 1;\n' });
  sh(work, 'add', '.');
  sh(work, 'commit', '-qm', 'base');
  await write({ 'app.ts': 'export const a = 2;\n' });
  sh(work, 'commit', '-qam', 'change');
  const oldHead = sh(work, 'rev-parse', 'HEAD');
  sh(work, 'checkout', '-q', '-b', 'moved', 'HEAD~1');
  await write({ 'other.ts': 'export const b = 1;\n' });
  sh(work, 'add', '.');
  sh(work, 'commit', '-qm', 'base moves on');
  const tip = sh(work, 'rev-parse', 'HEAD');
  await write({ 'app.ts': `export const a = ${change};\n` });
  sh(work, 'commit', '-qam', 'change, rebased');
  const newHead = sh(work, 'rev-parse', 'HEAD');
  sh(root, 'init', '-q', '--bare', 'acme/widgets.git');
  sh(work, 'push', '-q', join(root, 'acme/widgets.git'), `${tip}:refs/heads/main`, `${newHead}:refs/pull/7/head`, `${oldHead}:refs/heads/old`);
  return { base: `file://${root}`, oldHead, newHead };
}

type Seed = { outcome?: string; passed?: boolean; reviewed?: string };

// Runs one checkout_pr on the new head in a session that already posted a review of the old one.
async function checkoutAfter(change: string, seed: Seed = {}) {
  const { base, oldHead, newHead } = await rebased(change);
  const service = await fakeService(newHead);
  let result = '';
  const faux = fauxProvider({ models: [{ id: 'w' }] });
  faux.setResponses([
    fauxAssistantMessage(fauxToolCall('checkout_pr', { repository: 'acme/widgets', number: 7 }), { stopReason: 'toolUse' }),
    (c) => { result = JSON.stringify(c.messages.at(-1)); return fauxAssistantMessage('done'); },
  ]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const tool = checkoutTool({ serviceUrl: service.url, serviceToken: 'svc', gitBaseUrl: base });
  const session = await openSession({ dir: await tmp(), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'gh', tools: [tool] })] } });
  await session.root.commit(async (tx) => {
    const kinds = (await tx.doc(Artifacts, session.root.id)).kinds;
    kinds['github.review'] = { versions: [{ kind: 'github.review', version: 1, hash: 'h', content: 'x', sealedAt: 1, outcome: seed.outcome ?? 'approve', links: [{ label: 'Posted', url: REVIEW }], verdict: { passed: seed.passed ?? true, checks: [] } as never }] };
    const d = await tx.doc(PullRequestDoc, session.root.id);
    d.posted = [1];
    d.reviewed = { 1: seed.reviewed ?? oldHead };
  }, BACKGROUND_CONTEXT);
  await session.submit('Review acme/widgets#7', 'carry-1', true);
  const versions = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))!.kinds['github.review'].versions;
  await session.close();
  return { result, service, versions, oldHead, newHead };
}

test('a rebase with the same patch-id carries the approval to the new head without a review', async () => {
  const { result, service, versions, oldHead, newHead } = await checkoutAfter('2');
  assert.equal(service.posted.length, 1);
  assert.equal(service.posted[0].event, 'APPROVE');
  assert.equal(service.posted[0].commitId, newHead);
  assert.equal(service.posted[0].body, `Same change as the approved review of [${oldHead.slice(0, 8)}](${REVIEW}) (rebase only); approval carried forward`);
  assert.match(result, /do not review this head again/);
  assert.equal(versions.length, 2);
  assert.equal(versions[1].outcome, 'approve');
  assert.equal(versions[1].verdict, null);
  assert.deepEqual(versions[1].carriedFrom, { sha: oldHead, url: REVIEW });
  assert.equal(versions[1].links?.[0].url, 'https://github.com/acme/widgets/pull/7#review-9');
});

test('a changed line runs the full review', async () => {
  const { result, service, versions } = await checkoutAfter('3');
  assert.equal(service.posted.length, 0);
  assert.match(result, /Checked out/);
  assert.equal(versions.length, 1);
});

test('a prior request_changes runs the full review even when the change is identical', async () => {
  const { result, service } = await checkoutAfter('2', { outcome: 'request_changes' });
  assert.equal(service.posted.length, 0);
  assert.match(result, /Checked out/);
});

test('an approval released at the revision limit is not carried', async () => {
  const { service } = await checkoutAfter('2', { passed: false });
  assert.equal(service.posted.length, 0);
});

test('a patch-id failure runs the full review', async () => {
  const { result, service } = await checkoutAfter('2', { reviewed: '0'.repeat(40) });
  assert.equal(service.posted.length, 0);
  assert.match(result, /Checked out/);
});
