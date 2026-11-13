import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { gate, SealRefused, whileWaiting, type GateVerdict, type SealedArtifact } from '../src/index.ts';
import { runSealGates } from '../src/gates.ts';

const sealed: SealedArtifact = { kind: 'note', version: 2, hash: 'h', content: 'ducks', sealedAt: 0, verdict: null };

// The sealing task's api as the chain uses it: its clock and its memo, which a replay of the task shares.
function taskApi(memo = new Map<string, unknown>()) {
  const api = {
    now: () => 0,
    memo: async (name: string, ...rest: unknown[]) => {
      if (rest.length === 2 && !memo.has(name)) memo.set(name, rest[0]);
      return memo.get(name);
    },
  };
  return { api: api as unknown as ToolExecutionApi, memo };
}

// Registers gates for one test and removes them after, since registration is module state.
function withGates(t: { after(fn: () => void): void }, ...runs: Array<() => Promise<GateVerdict | void>>) {
  const offs = runs.map((run) => gate('artifact.sealed', run));
  t.after(() => offs.forEach((off) => off()));
}

test('gates run in registration order, and every pass note comes back in that order', async (t) => {
  const ran: string[] = [];
  withGates(t, async () => (ran.push('a'), { kind: 'pass', note: 'checked' }), async () => void ran.push('b'), async () => (ran.push('c'), { kind: 'pass', note: 'posted' }));
  assert.deepEqual(await runSealGates(taskApi().api, sealed, BACKGROUND_CONTEXT), ['checked', 'posted']);
  assert.deepEqual(ran, ['a', 'b', 'c']);
});

test('the first continue or deny wins and the later gates do not run', async (t) => {
  let later = 0;
  withGates(t, async () => ({ kind: 'continue', feedback: 'fix the verdict line' }), async () => void later++);
  const err = await runSealGates(taskApi().api, sealed, BACKGROUND_CONTEXT).catch((e) => e);
  assert.ok(err instanceof SealRefused);
  assert.deepEqual(err.verdict, { kind: 'continue', feedback: 'fix the verdict line' });
  assert.equal(err.message, 'fix the verdict line');
  assert.equal(later, 0);
});

test('a replay of the sealing task gets the recorded verdict without running the gate; a re-seal in one run reruns it', async (t) => {
  let runs = 0;
  withGates(t, async () => ({ kind: 'pass', note: `run ${++runs}` }));
  const first = taskApi();
  assert.deepEqual(await runSealGates(first.api, sealed, BACKGROUND_CONTEXT), ['run 1']);
  assert.deepEqual(await runSealGates(first.api, sealed, BACKGROUND_CONTEXT), ['run 2'], 'the same version sealed again runs the chain again');
  const replay = taskApi(first.memo);
  assert.deepEqual(await runSealGates(replay.api, sealed, BACKGROUND_CONTEXT), ['run 1']);
  assert.deepEqual(await runSealGates(replay.api, sealed, BACKGROUND_CONTEXT), ['run 2']);
  assert.equal(runs, 2);
});

// Lets the chain's pending callbacks run between timer ticks.
const settle = async () => {
  for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
};

test('a gate over its bound is a deny: fail-closed', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
  withGates(t, () => new Promise(() => {}));
  const chain = runSealGates(taskApi().api, sealed, BACKGROUND_CONTEXT).catch((e) => e);
  await settle();
  t.mock.timers.tick(300_000);
  const err = await chain;
  assert.ok(err instanceof SealRefused);
  assert.deepEqual(err.verdict, { kind: 'deny', reason: 'gate timed out' });
});

test('time a gate spends waiting on a person does not count against its bound', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
  let release!: () => void;
  const person = new Promise<void>((r) => (release = r));
  t.after(gate('artifact.sealed', () => whileWaiting(() => person).then(() => ({ kind: 'pass' as const, note: 'approved' })), { timeoutMs: 1000 }));
  const chain = runSealGates(taskApi().api, sealed, BACKGROUND_CONTEXT);
  await settle();
  t.mock.timers.tick(10_000);
  release();
  assert.deepEqual(await chain, ['approved']);
});

test('only artifact.sealed takes gates so far', () => {
  assert.throws(() => gate('turn.ended', async () => undefined), /not wired yet/);
});
