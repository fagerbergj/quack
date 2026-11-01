import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { createSessionServer, openSession } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-session-'));
const clockDir = await mkdtemp(join(tmpdir(), 'quack-clock-'));
after(() => rm(clockDir, { recursive: true, force: true }));
after(() => rm(dir, { recursive: true, force: true }));

function fakeModels(...answers: string[]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(answers.map((a) => fauxAssistantMessage(a)));
  const models = createModels();
  models.setProvider(faux.provider);
  const model = faux.getModel();
  return { models, agent: { model: { provider: model.provider, modelId: model.id } } };
}

async function serve(session: Awaited<ReturnType<typeof openSession>>) {
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const post = (body: unknown) =>
    fetch(`${base}/invocations`, { method: 'POST', body: JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json() }));
  return { base, post, stop: () => new Promise((r) => server.close(r)) };
}

test('answers over the container contract and survives a restart', async () => {
  const first = await openSession({ dir, ...fakeModels('Paris') });
  const http = await serve(first);

  const ping = await fetch(`${http.base}/ping`).then((r) => r.json());
  assert.deepEqual(ping, { status: 'Healthy', waitingOnHuman: false, wake: {} });

  const answered = await http.post({ input: 'Capital of France?', requestId: 'q-1', wait: true });
  assert.equal(answered.status, 200);
  assert.equal(answered.body.status, 'done');
  assert.equal(answered.body.answer, 'Paris');

  const rejected = await http.post({ input: '' });
  assert.equal(rejected.status, 400);

  await http.stop();
  await first.close();

  // A new process on the same storage keeps the transcript and dedupes a retried request id (HAR-3, HAR-5).
  const second = await openSession({ dir, ...fakeModels() });
  const retried = await second.submit('Capital of France?', 'q-1', true);
  assert.equal(retried.submissionId, answered.body.submissionId);
  assert.equal(retried.status, 'done');
  await second.close();
});

test('every run sees the date in the deployment zone, and a run resumed on a later day sees that day', async () => {
  const seen: string[] = [];
  const open = (now: string) => {
    const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
    faux.setResponses([(ctx) => (seen.push(JSON.stringify(ctx.messages)), fauxAssistantMessage('ok'))]);
    const models = createModels();
    models.setProvider(faux.provider);
    const model = faux.getModel();
    return openSession({ dir: clockDir, models, agent: { model: { provider: model.provider, modelId: model.id } }, timeZone: 'Asia/Tokyo', now: () => new Date(now) });
  };
  const first = await open('2026-11-01T20:00:00Z');
  await first.submit('hi', 'd-1', true);
  await first.close();
  const second = await open('2026-11-03T01:00:00Z');
  await second.submit('again', 'd-2', true);
  await second.close();
  assert.match(seen[0], /Today is Monday, 2026-11-02 \(Asia\/Tokyo\)\./);
  assert.match(seen[1], /Today is Tuesday, 2026-11-03 \(Asia\/Tokyo\)\./);
});
