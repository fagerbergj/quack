import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { defineExtension } from '@earendil-works/pi-durable';
import { openSession } from '@quack/session';
import { checkoutTool, PullRequestDoc } from '../src/index.ts';
import { cleanup, fakeService, remotes, sh, tmp } from './fixtures.ts';

after(cleanup);

test('checks out the PR head with its base, records the PR, and is safe to call again', async () => {
  const { base, head } = await remotes();
  const serviceUrl = (await fakeService(head)).url;
  let result = '';
  const faux = fauxProvider({ models: [{ id: 'w' }] });
  const call = () => fauxAssistantMessage(fauxToolCall('checkout_pr', { repository: 'acme/widgets', number: 7 }), { stopReason: 'toolUse' });
  faux.setResponses([call(), call(), (c) => { result = JSON.stringify(c.messages.at(-1)); return fauxAssistantMessage('Checked out.'); }]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const tool = checkoutTool({ serviceUrl, serviceToken: 'svc', gitBaseUrl: base });
  const session = await openSession({ dir: await tmp(), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'gh', tools: [tool] })] } });

  const done = await session.submit('Review acme/widgets#7', 'co-1', true);
  assert.equal(done.status === 'done' && done.answer, 'Checked out.');
  assert.equal(sh(join(session.workspace, 'repo'), 'rev-parse', 'HEAD'), head);
  assert.match(sh(join(session.workspace, 'repo'), 'diff', 'origin/main...HEAD'), /-export const a = 1;\n\+export const a = 2;/);
  assert.match(result, /Fixes #3/);
  assert.match(result, /CI checks: ci=success/);
  assert.match(result, /Linked issue #3: a should be 2/);
  assert.match(result, /Comments stay at two lines/, 'conventions from AGENTS.md');
  const doc = await session.harness.snapshot(PullRequestDoc, session.root.id, BACKGROUND_CONTEXT);
  assert.equal(doc!.pr!.headSha, head);
  await session.close();
});

test('the askpass helper answers git with a fresh token from the service', async () => {
  const serviceUrl = (await fakeService('x')).url;
  const askpass = fileURLToPath(new URL('../src/askpass.mjs', import.meta.url));
  const env = { ...process.env, QUACK_GH_SERVICE_URL: serviceUrl, QUACK_GH_SERVICE_TOKEN: 'svc', QUACK_GH_REMOTE: 'https://github.com/acme/widgets.git' };
  // Async: the fake service runs in this process, so a blocking spawn would deadlock against it.
  const run = async (prompt: string, extra = {}) => (await promisify(execFile)(askpass, [prompt], { env: { ...env, ...extra }, encoding: 'utf8' })).stdout.trim();
  assert.equal(await run("Username for 'https://github.com': "), 'x-access-token');
  assert.equal(await run("Password for 'https://x-access-token@github.com': "), 'ghs_from_service');
  await assert.rejects(run('Password: ', { QUACK_GH_SERVICE_TOKEN: 'wrong' }));
});
