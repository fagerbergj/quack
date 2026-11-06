import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
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
  assert.deepEqual(ping, { status: 'Healthy', pid: process.pid, waitingOnHuman: false, wake: {} });

  const answered = await http.post({ input: 'Capital of France?', requestId: 'q-1', wait: true });
  assert.equal(answered.status, 200);
  assert.equal(answered.body.status, 'done');
  assert.equal(answered.body.answer, 'Paris');

  const titled = await http.post({ input: 'next', title: 'Fix the cache', requestId: 'q-2', wait: true });
  assert.equal(titled.status, 200);
  await http.post({ input: 'again', title: 'Later name', requestId: 'q-3', wait: true });
  assert.equal(await readFile(join(dir, 'title'), 'utf8'), 'Fix the cache');
  assert.equal((await http.post({ input: 'x', title: 7 })).status, 400);

  // Meta rides beside the input; a record with the same id replaces the earlier one.
  await http.post({ input: 'm1', meta: { id: 'a', files: [] }, requestId: 'q-4', wait: true });
  await http.post({ input: 'm2', meta: { id: 'b' }, requestId: 'q-5', wait: true });
  await http.post({ input: 'm3', meta: { id: 'a', files: [1] }, requestId: 'q-6', wait: true });
  assert.deepEqual(await fetch(`${http.base}/meta`).then((r) => r.json()), { meta: [{ id: 'b' }, { id: 'a', files: [1] }] });
  assert.equal((await http.post({ input: 'x', meta: { files: [] } })).status, 400);

  // Meta never gates a run: a corrupt meta.json, then a write that fails, both still submit.
  await writeFile(join(dir, 'meta.json'), '[{"id": "a", ');
  assert.equal((await http.post({ input: 'm4', meta: { id: 'c' }, requestId: 'q-7', wait: true })).status, 200);
  assert.deepEqual(await fetch(`${http.base}/meta`).then((r) => r.json()), { meta: [{ id: 'c' }] });
  await mkdir(join(dir, 'meta.json.tmp'));
  assert.equal((await http.post({ input: 'm5', meta: { id: 'd' }, requestId: 'q-8', wait: true })).status, 200);
  await rm(join(dir, 'meta.json.tmp'), { recursive: true });

  const rejected = await http.post({ input: '' });
  assert.equal(rejected.status, 400);

  await http.stop();
  await first.close();

  // A new process on the same storage keeps the transcript and dedupes a retried request id.
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

test('meta records arriving together all land, one per id, and leave no temp files', async () => {
  const own = await mkdtemp(join(tmpdir(), 'quack-meta-'));
  after(() => rm(own, { recursive: true, force: true }));
  const session = await openSession({ dir: own, ...fakeModels() });
  await Promise.all(['files', 'checks', 'files', 'labels'].map((id, i) => session.recordMeta({ id, n: i })));
  assert.deepEqual((await session.meta()).map((r) => [r.id, r.n]).sort(), [['checks', 1], ['files', 2], ['labels', 3]]);
  assert.deepEqual((await readdir(own)).filter((f) => f.startsWith('meta')), ['meta.json']);
  await session.close();
});
