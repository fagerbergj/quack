import assert from 'node:assert/strict';
import type { AddressInfo } from 'node:net';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, retryDelayMs, type FauxResponseStep } from '@earendil-works/pi-ai';
import type { ConversationRetryPolicy } from '@earendil-works/pi-durable';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { failedRows, Failures, retryEngineBusy } from '@quack/sdk';
import { createSessionServer, MODEL_OUTAGE_HOURS, openSession, outageRetry } from '../src/index.ts';

const root = await mkdtemp(join(tmpdir(), 'quack-retry-'));
after(() => rm(root, { recursive: true, force: true }));

const fail = (message: string) => fauxAssistantMessage('', { stopReason: 'error', errorMessage: message });
const bad502 = () => fail('502 peer proxy error: dial tcp 10.0.0.2:11436: connect: no route to host');
const FAST = { baseDelayMs: 1, maxAgentDelayMs: 1 };

async function open(name: string, steps: FauxResponseStep[], retry: Partial<ConversationRetryPolicy> = FAST) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(steps);
  const models = createModels();
  models.setProvider(retryEngineBusy(faux.provider));
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

test('a full K/V pool is retried like a transient refusal', async () => {
  const { session, faux } = await open('pool-full', [fail('prompt read (BGEN slot 1): the K/V pool is full (153729 positions on this lane, 801 more pages wanted, 761 free)'), fauxAssistantMessage('Paris')]);
  const r = await session.submit('Capital of France?', 'q', true);
  assert.deepEqual([r.status, faux.state.callCount], ['done', 2]);
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

test('the production policy waits out about six hours of outage, trying at least every five minutes', () => {
  const policy = outageRetry();
  let total = 0;
  for (let n = 1; n <= policy.maxRetries; n++) total += retryDelayMs(policy, n);
  assert.ok(total <= MODEL_OUTAGE_HOURS * 3_600_000 && total > MODEL_OUTAGE_HOURS * 3_600_000 - 300_000, `total backoff ${total}ms`);
  assert.equal(retryDelayMs(policy, policy.maxRetries), 300_000);
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
  const waiting = { since: standing.waiting!.since, ahead: 0, provider: 'faux', error: bad502().errorMessage };
  assert.deepEqual(ping, { status: 'Healthy', pid: process.pid, waitingOnHuman: false, wake: { at: standing.wakeAt }, waiting, note: `Waiting for faux to answer: ${waiting.error}` });
  await new Promise((r) => server.close(r));
  await session.close();
});

const abort = () => fail('Provider finish_reason: abort');
type Opened = Awaited<ReturnType<typeof open>>;
const failures = async (s: Opened['session']) => failedRows(await s.harness.snapshot(Failures, s.root.id, BACKGROUND_CONTEXT));
const answered = async (s: Opened['session']) => (await s.messages(5)).some((m) => m.text === 'Paris');

async function awaitWatch(watch: { start: (cb: () => Promise<void>) => void; stop: () => Promise<unknown> }, check: () => Promise<boolean>) {
  if (await check()) return void (await watch.stop());
  await new Promise<void>((resolve) => {
    watch.start(async () => { if (await check()) resolve(); });
  });
  await watch.stop();
}

const waitAnswer = async (s: Opened['session']) => awaitWatch(await s.root.watch(BACKGROUND_CONTEXT), () => answered(s));
const waitFailure = async (s: Opened['session']) => {
  await s.root.commit(async (tx) => { await tx.doc(Failures, s.root.id); }, BACKGROUND_CONTEXT);
  const watch = (await s.harness.watchDoc(Failures, s.root.id, BACKGROUND_CONTEXT))!;
  await awaitWatch(watch, async () => (await failures(s)).length > 0);
};

test('a provider abort is retried once and the run completes without a failure', async () => {
  const { session, faux } = await open('abort-once', [abort(), fauxAssistantMessage('Paris')]);
  await session.submit('Capital of France?', 'q');
  await waitAnswer(session);
  assert.equal(faux.state.callCount, 2);
  assert.deepEqual(await failures(session), []);
  await session.close();
});

test('a second abort records a failure that resume accepts, and the rerun clears it', async () => {
  const { session, faux } = await open('abort-twice', [abort(), abort(), fauxAssistantMessage('Paris')]);
  await session.submit('Capital of France?', 'q');
  await waitFailure(session);
  assert.match((await failures(session))[0].error, /abort/);
  assert.equal(faux.state.callCount, 2);
  const failed = (await session.harness.snapshot(Failures, session.root.id, BACKGROUND_CONTEXT))!.items.model;
  assert.ok(failed.run > 0);
  assert.equal((await session.resume() as { status: string }).status, 'placed');
  await waitAnswer(session);
  assert.deepEqual(await failures(session), []);
  // The index orders a row by its run, so the clear must outrank the failure it supersedes.
  const cleared = (await session.harness.snapshot(Failures, session.root.id, BACKGROUND_CONTEXT))!.items.model;
  assert.ok(cleared.run > failed.run);
  assert.equal(faux.state.callCount, 3);
  await session.close();
});

test('an abort whose queued follow-up already started keeps its retry for that run', async () => {
  const { session, faux } = await open('abort-followup', [abort(), abort(), fauxAssistantMessage('Paris')]);
  await session.submit('first', 'q1');
  await session.submit('second', 'q2', false, 'followUp');
  // The aborted run's single retry consumes the second abort and fails that run, before the follow-up answers.
  await waitFailure(session);
  const rows = await failures(session);
  assert.equal(rows.length, 1);
  assert.match(rows[0].error, /abort/);
  await waitAnswer(session);
  assert.equal(faux.state.callCount, 3);
  await session.close();
});

test('two concurrent resumes do not throw', async () => {
  const { session } = await open('double-resume', [abort(), abort(), fauxAssistantMessage('Paris')]);
  await session.submit('Capital of France?', 'q');
  await waitFailure(session);
  const results = await Promise.all([session.resume(), session.resume()]);
  assert.ok(results.some((r) => typeof r === 'object' && r?.status === 'placed'));
  await session.close();
});

test('a model failure present at open is recorded, not retried', async () => {
  const first = await open('crash-window', [abort(), abort()]);
  await first.session.submit('hi', 'q');
  await waitFailure(first.session);
  await first.session.root.commit(async (tx) => { delete (await tx.doc(Failures, first.session.root.id)).items.model; }, BACKGROUND_CONTEXT);
  await first.session.close();
  const again = await open('crash-window', [fauxAssistantMessage('never')]);
  assert.equal((await failures(again.session)).length, 1);
  assert.equal(again.faux.state.callCount, 0);
  await again.session.close();
});

test('a non-abort model error records a failure at once without a rerun', async () => {
  const { session, faux } = await open('bad-request-failure', [fail('400 invalid request'), fauxAssistantMessage('never')]);
  await session.submit('hi', 'q', true);
  await waitFailure(session);
  assert.equal(faux.state.callCount, 1);
  assert.match((await failures(session))[0].error, /400/);
  await session.close();
});
