import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT, withAbortSignal } from '@earendil-works/chord/context';
import { answerUnit } from '../src/resolve.ts';

const ref = { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 };
const unit = { id: 'f1', state: { finding: '- nit: rename' } };
const checks = [{ id: 'c.says_why', label: 'says why', weight: 2, question: 'Does it say why?' }];
const memoKey = 'judge:github.review:v1:f1:classifier';

// A tool call's api as answerUnit sees it: its memos survive a rerun of the call, so one api stands for one call.
function callApi(classify: () => Promise<unknown>) {
  const memos = new Map<string, unknown>([['decision:judge:github.review:v1:f1:c.says_why', { choice: 'yes', confidence: 0.9, resolver: 'agent-model' }]]);
  let calls = 0;
  const api = {
    memos,
    calls: () => calls,
    async memo(name: string, ...rest: unknown[]) {
      if (rest.length === 2 && !memos.has(name)) memos.set(name, rest[0]);
      return memos.get(name);
    },
    models: {
      getAllModels: () => [{ id: 'clef', type: 'classifier' }],
      classify: () => (calls++, classify()),
    },
  };
  return api;
}

const confident = { stopReason: 'stop', answers: { 'c.says_why': { type: 'bool', probability: 0.95 } } };

test('a classifier that throws is not memoized: a rerun of the call asks it again', async () => {
  const outcomes = [() => Promise.reject(new Error('503 busy')), () => Promise.resolve({ stopReason: 'error', answers: {} }), () => Promise.resolve(confident)];
  const api = callApi(() => outcomes.shift()!());
  const first = await answerUnit(api as never, ref, undefined, 'judge:github.review:v1', unit, checks, BACKGROUND_CONTEXT);
  assert.equal(first['c.says_why'].resolver, 'agent-model', 'an outage escalates this time');
  assert.equal(api.memos.has(memoKey), false);
  await answerUnit(api as never, ref, undefined, 'judge:github.review:v1', unit, checks, BACKGROUND_CONTEXT);
  assert.equal(api.memos.has(memoKey), false, 'an error stop is an outage too');
  const third = await answerUnit(api as never, ref, undefined, 'judge:github.review:v1', unit, checks, BACKGROUND_CONTEXT);
  assert.deepEqual([api.calls(), third['c.says_why'].resolver], [3, 'clef']);
});

test('an over-cap unit is memoized: a rerun escalates without asking the classifier', async () => {
  const api = callApi(() => Promise.resolve(confident));
  const tiny = { ...ref, maxInputTokens: 1 };
  await answerUnit(api as never, tiny, undefined, 'judge:github.review:v1', unit, checks, BACKGROUND_CONTEXT);
  assert.equal(api.memos.get(memoKey), null);
  await answerUnit(api as never, ref, undefined, 'judge:github.review:v1', unit, checks, BACKGROUND_CONTEXT);
  assert.equal(api.calls(), 0, 'the memoized miss stands even once the unit would fit');
});

test('an abort propagates instead of escalating', async () => {
  const ac = new AbortController();
  const api = callApi(() => {
    ac.abort(new Error('closing'));
    return Promise.reject(new DOMException('aborted', 'AbortError'));
  });
  await assert.rejects(answerUnit(api as never, ref, undefined, 'judge:github.review:v1', unit, checks, withAbortSignal(ac.signal, BACKGROUND_CONTEXT)), /closing/);
  assert.equal(api.memos.has(memoKey), false);
});
