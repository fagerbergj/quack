import assert from 'node:assert/strict';
import type { AddressInfo } from 'node:net';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, retryDelayMs, type FauxResponseStep } from '@earendil-works/pi-ai';
import { createSessionServer, MODEL_RETRY, openSession } from '../src/index.ts';

const root = await mkdtemp(join(tmpdir(), 'quack-retry-'));
after(() => rm(root, { recursive: true, force: true }));

const fail = (message: string) => fauxAssistantMessage('', { stopReason: 'error', errorMessage: message });
const bad502 = () => fail('502 peer proxy error: dial tcp 10.0.0.2:11436: connect: no route to host');
const FAST = { baseDelayMs: 1, maxAgentDelayMs: 1 };

async function open(name: string, steps: FauxResponseStep[], retry: Partial<typeof MODEL_RETRY> = FAST) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(steps);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: join(root, name), models, agent: { model: { provider: m.provider, modelId: m.id } }, retry });
  return { session, faux };
}

test('two 502s then success completes the run', async () => {
  const { session, faux } = await open('transient', [bad502(), bad502(), fauxAssistantMessage('Paris')]);
  const r = await session.submit('Capital of France?', 'q', true);
  assert.deepEqual([r.status, faux.state.callCount], ['done', 3]);
  await session.close();
});

test('a 400 fails at once as model_error', async () => {
  const { session, faux } = await open('bad-request', [fail('400 invalid request'), fauxAssistantMessage('never')]);
  const r = await session.submit('hi', 'q', true);
  assert.equal(r.status, 'unanswered');
  assert.equal((r as { reason: string }).reason, 'model_error');
  assert.equal(faux.state.callCount, 1);
  await session.close();
});

test('retries are bounded: an exhausted budget fails as model_error', async () => {
  const { session, faux } = await open('exhausted', [bad502(), bad502(), bad502(), fauxAssistantMessage('never')], { ...FAST, maxRetries: 2 });
  const r = await session.submit('hi', 'q', true);
  assert.equal((r as { reason: string }).reason, 'model_error');
  assert.equal(faux.state.callCount, 3);
  await session.close();
});

test('the production policy rides out about five minutes of outage', () => {
  let total = 0;
  for (let n = 1; n <= MODEL_RETRY.maxRetries; n++) total += retryDelayMs(MODEL_RETRY, n);
  assert.ok(total >= 240_000 && total <= 360_000, `total backoff ${total}ms`);
});

test('a submission queued behind a failing run is run, not left queued', async () => {
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  let started!: () => void;
  const running = new Promise<void>((r) => (started = r));
  const { session } = await open('queued', [async () => (started(), await gate, fail('400 invalid request')), fauxAssistantMessage('second')]);
  const first = session.submit('first', 'a', true);
  await running;
  const second = session.submit('second', 'b', true);
  release();
  assert.equal((await first).status, 'unanswered');
  const r = await second;
  assert.deepEqual([r.status, r.status === 'done' && r.answer], ['done', 'second']);
  await session.close();
});

test('a session in retry backoff reports its wake time, so the distributor can stop and wake it', async () => {
  let started!: () => void;
  const running = new Promise<void>((r) => (started = r));
  const { session } = await open('backoff', [() => (started(), bad502()), fauxAssistantMessage('ok')], { baseDelayMs: 3_600_000, maxAgentDelayMs: 3_600_000 });
  void session.submit('hi', 'q');
  await running;
  while (session.harness.wake().at === undefined) await new Promise((r) => setImmediate(r));
  assert.equal(session.harness.wake().active, false);
  const standing = await session.standing();
  assert.deepEqual([standing.busy, standing.wakeAt], [false, session.harness.wake().at]);
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const ping = await fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/ping`).then((r) => r.json());
  assert.deepEqual(ping, { status: 'Healthy', waitingOnHuman: false, wake: { at: standing.wakeAt } });
  await new Promise((r) => server.close(r));
  await session.close();
});
