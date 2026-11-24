import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, type FauxResponseStep } from '@earendil-works/pi-ai';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import type { SetTimer } from '@earendil-works/pi-durable';
import { failedRows, Failures, retryEngineBusy } from '@quack/sdk';
import { openSession } from '../src/index.ts';

const root = await mkdtemp(join(tmpdir(), 'quack-outage-'));
after(() => rm(root, { recursive: true, force: true }));

// The Harness clock and timers, moved by hand: backoffs of minutes pass in a few event-loop turns.
class Clock {
  t = 1_700_000_000_000;
  #timers: Array<{ at: number; fn: () => void }> = [];
  now = () => new Date(this.t);
  setTimer: SetTimer = (fn, ms) => {
    const timer = { at: this.t + ms, fn };
    this.#timers.push(timer);
    return () => void (this.#timers = this.#timers.filter((x) => x !== timer));
  };
  async settle() {
    for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
  }
  async advance(ms: number) {
    const end = this.t + ms;
    for (;;) {
      const next = this.#timers.filter((x) => x.at <= end).sort((a, b) => a.at - b.at)[0];
      if (!next) break;
      this.#timers = this.#timers.filter((x) => x !== next);
      this.t = Math.max(this.t, next.at);
      next.fn();
      await this.settle();
    }
    this.t = end;
    await this.settle();
  }
  // Moves a minute at a time until `done`, up to `limitMs` of Harness time.
  async until(done: () => Promise<boolean>, limitMs: number) {
    for (let waited = 0; waited <= limitMs; waited += 60_000) {
      if (await done()) return true;
      await this.advance(60_000);
    }
    return done();
  }
}

const refused = () => fauxAssistantMessage('', { stopReason: 'error', errorMessage: 'Connection error.' });
const noRoute = () => fauxAssistantMessage('', { stopReason: 'error', errorMessage: '502 peer proxy error: dial tcp 10.0.0.2:11436: connect: no route to host' });
// Longer than the old budget of nine retries (about five minutes of backoff) by far.
const OUTAGE = [...Array.from({ length: 8 }, refused), ...Array.from({ length: 8 }, noRoute)];
const HOUR = 3_600_000;

async function open(name: string, steps: FauxResponseStep[], clock: Clock, outageHours?: number, turnCeiling?: number) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(steps);
  const models = createModels();
  models.setProvider(retryEngineBusy(faux.provider));
  const m = faux.getModel();
  const session = await openSession({ dir: join(root, name), models, agent: { model: { provider: m.provider, modelId: m.id } }, now: clock.now, setTimer: clock.setTimer, outageHours, turnCeiling });
  const said = async (text: string) => (await session.messages(10)).some((x) => x.role === 'assistant' && x.text === text);
  return { session, faux, provider: m.provider, said };
}

test('a model refusing connections for far longer than five minutes parks the run as waiting, then it answers and the queued input runs', async () => {
  const clock = new Clock();
  const { session, faux, provider, said } = await open('recovers', [...OUTAGE, fauxAssistantMessage('Paris'), fauxAssistantMessage('Rome')], clock);
  await session.submit('Capital of France?', 'q1');
  assert.ok(await clock.until(async () => faux.state.callCount >= 3, HOUR));
  const parked = (await session.standing()).waiting;
  assert.deepEqual([parked?.provider, parked?.ahead, parked?.error], [provider, 0, 'Connection error.']);
  assert.match(String((await session.standing()).note), new RegExp(`Waiting for ${provider} to answer`));
  await session.submit('Capital of Italy?', 'q2', false, 'followUp');
  assert.ok(await clock.until(() => said('Rome'), 2 * HOUR), 'the outage ends and both inputs are answered with no new message');
  assert.ok(await said('Paris'));
  assert.equal(faux.state.callCount, OUTAGE.length + 2);
  assert.equal((await session.standing()).waiting, null);
  assert.deepEqual(failedRows(await session.harness.snapshot(Failures, session.root.id, BACKGROUND_CONTEXT)), []);
  await session.close();
});

test('an endpoint still failing past the outage window ends the run with an error naming it and how long it failed', async () => {
  const clock = new Clock();
  const { session, provider } = await open('ceiling', Array.from({ length: 40 }, refused), clock, 0.5);
  await session.submit('hi', 'q');
  const rows = async () => failedRows(await session.harness.snapshot(Failures, session.root.id, BACKGROUND_CONTEXT));
  assert.ok(await clock.until(async () => (await rows()).length > 0, 2 * HOUR));
  assert.match((await rows())[0].error, new RegExp(`^${provider} kept failing for \\d+ min, so the run ended\\. Run it again once ${provider} answers\\. Last error: Connection error\\.$`));
  assert.equal((await session.standing()).waiting, null);
  await session.close();
});

test('a 400 fails at once under the outage policy, with no backoff', async () => {
  const clock = new Clock();
  const { session, faux } = await open('bad-request', [fauxAssistantMessage('', { stopReason: 'error', errorMessage: '400 invalid request' }), fauxAssistantMessage('never')], clock);
  const r = await session.submit('hi', 'q', true);
  assert.deepEqual([r.status, (r as { reason?: string }).reason, faux.state.callCount], ['unanswered', 'model_error', 1]);
  await session.close();
});

test('a stop during a park ends the run, tries the model no more and clears the wait', async () => {
  const clock = new Clock();
  const { session, faux } = await open('stopped', [...OUTAGE, fauxAssistantMessage('never')], clock);
  const sent = await session.submit('hi', 'q');
  assert.ok(await clock.until(async () => faux.state.callCount >= 12, HOUR), 'parked past the old budget');
  const calls = faux.state.callCount;
  const stopped = await session.abort('service');
  assert.deepEqual(stopped.aborted, [sent.submissionId]);
  assert.equal((await session.standing()).waiting, null);
  await clock.advance(HOUR);
  assert.equal(faux.state.callCount, calls);
  await session.close();
});

test('a session reopened while parked is still waiting, and answers once the model is back', async () => {
  const clock = new Clock();
  const first = await open('restart', OUTAGE, clock);
  await first.session.submit('Capital of France?', 'q');
  assert.ok(await clock.until(async () => first.faux.state.callCount >= 12, HOUR), 'parked past the old budget');
  await first.session.close();
  const again = await open('restart', [fauxAssistantMessage('Paris')], clock);
  assert.equal((await again.session.standing()).waiting?.provider, again.provider);
  assert.ok(await clock.until(() => again.said('Paris'), HOUR));
  assert.equal(again.faux.state.callCount, 1);
  await again.session.close();
});

test('failed tries are not model turns: a long park does not trip the run ceiling', async () => {
  const clock = new Clock();
  const { session, said } = await open('ceiling-turns', [...OUTAGE, fauxAssistantMessage('Paris')], clock, undefined, 3);
  await session.submit('Capital of France?', 'q');
  assert.ok(await clock.until(() => said('Paris'), 2 * HOUR));
  assert.deepEqual(failedRows(await session.harness.snapshot(Failures, session.root.id, BACKGROUND_CONTEXT)), []);
  await session.close();
});
