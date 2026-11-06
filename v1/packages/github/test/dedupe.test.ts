import assert from 'node:assert/strict';
import { createHmac } from 'node:crypto';
import { after, test } from 'node:test';
import { fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { createModels } from '@earendil-works/pi-ai';
import { serveApp } from '@quack/sdk';
import { createSessionServer, openSession } from '@quack/session';
import type { GithubApp } from '../src/app.ts';
import { createGithubService } from '../src/service.ts';
import { cleanup, listen, tmp } from './fixtures.ts';

after(cleanup);

test('a redelivered event carries the original delivery id, so it is one submission in the session', async () => {
  let runs = 0;
  const faux = fauxProvider({ models: [{ id: 'm' }] });
  faux.setResponses(Array.from({ length: 4 }, () => () => (runs++, fauxAssistantMessage('Reviewed.'))) as never);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: await tmp(), models, agent: { model: { provider: m.provider, modelId: m.id } } });
  const sessionUrl = await listen(createSessionServer(session));
  // The distributor's routing is not under test: every submission lands on this one session, and each answer is kept.
  const answers: Array<{ submissionId: string }> = [];
  const toSession = (async (_url: string, init: RequestInit) => {
    const res = await fetch(`${sessionUrl}/invocations`, init);
    if (!JSON.parse(String(init.body)).wait) answers.push((await res.clone().json()) as { submissionId: string });
    return res;
  }) as unknown as typeof fetch;
  // A GitHub that knows the app's handle and nothing else: the opening card is skipped, which the webhook tolerates.
  const app = { slug: async () => 'quack-app', asInstallation: async () => ({ status: 404, json: undefined, link: '' }) } as unknown as GithubApp;
  const svc = createGithubService(app, { webhookSecret: 's', distributorUrl: 'http://distributor', fetch: toSession, label: 'quack:review', agent: 'reviewer' });
  const url = await listen(serveApp(svc));
  const event = JSON.stringify({ action: 'labeled', label: { name: 'quack:review' }, pull_request: { number: 7, title: 't', body: '', head: { sha: 'abc', ref: 'f' }, base: { ref: 'main' } }, repository: { name: 'widgets', owner: { login: 'acme' } } });
  const headers = { 'x-github-event': 'pull_request', 'x-hub-signature-256': `sha256=${createHmac('sha256', 's').update(event).digest('hex')}`, 'x-github-delivery': 'd-redo' };
  for (let i = 0; i < 2; i++) assert.equal((await fetch(`${url}/webhook`, { method: 'POST', body: event, headers })).status, 202);
  const done = await (await fetch(`${sessionUrl}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'x', requestId: 'github-delivery:d-redo', wait: true }) })).json();
  assert.equal(done.status, 'done');
  assert.equal(answers.length, 2);
  assert.equal(answers[0].submissionId, answers[1].submissionId, 'the second delivery found the first submission');
  assert.equal(done.submissionId, answers[0].submissionId);
  assert.equal(runs, 1, 'the model ran once');
  svc.stop();
  await session.close();
});
