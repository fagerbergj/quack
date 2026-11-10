import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT, withAbortSignal } from '@earendil-works/chord/context';
import type { Context } from '@earendil-works/chord/context';
import { limiter } from '../src/limit.ts';
import { answerItems, classifierTokens, pack, type ClassifierRef } from '../src/resolve.ts';

const ref = { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 };
const unit = { id: 'f1', state: { finding: '- nit: rename' } };
const checks = [{ id: 'c.says_why', question: { type: 'bool' as const, instructions: 'Does it say why?' } }];
const memoKey = 'judge:github.review:v1:f1:classifier';
// One unit judged alone, answers by check id.
const answerUnit = async (api: unknown, r: ClassifierRef, ctx: Context) =>
  Object.fromEntries((await answerItems(api as never, { ref: r, memo: 'judge:github.review:v1', clef: limiter(1), escalations: limiter(1) }, [{ unit, checks }], ctx)).map((a) => [a.check.id, a.answer]));

// A tool call's api as the judge sees it: its memos survive a rerun of the call, so one api stands for one call.
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

const confident = { stopReason: 'stop', answers: { q0: { type: 'bool', probability: 0.95 } } };

test('a classifier that throws is not memoized: a rerun of the call asks it again', async () => {
  const outcomes = [() => Promise.reject(new Error('503 busy')), () => Promise.resolve({ stopReason: 'error', answers: {} }), () => Promise.resolve(confident)];
  const api = callApi(() => outcomes.shift()!());
  const first = await answerUnit(api, ref, BACKGROUND_CONTEXT);
  assert.equal(first['c.says_why']!.resolver, 'agent-model', 'an outage escalates this time');
  assert.equal(api.memos.has(memoKey), false);
  await answerUnit(api, ref, BACKGROUND_CONTEXT);
  assert.equal(api.memos.has(memoKey), false, 'an error stop is an outage too');
  const third = await answerUnit(api, ref, BACKGROUND_CONTEXT);
  assert.deepEqual([api.calls(), third['c.says_why']!.resolver], [3, 'clef']);
});

test('a unit\'s questions go to the classifier numbered, never under their check ids, and each answer comes back to its check', async () => {
  const two = [{ id: 'c.no_self_contradiction', question: { type: 'choice' as const, instructions: 'Does any figure conflict?', options: { yes: 'Yes, this holds.', no: 'No, it does not hold.' } }, pass: ['no'] }, ...checks];
  let sent: { questions: Record<string, unknown> } | undefined;
  const api = callApi(async () => ({ stopReason: 'stop', answers: { q0: { type: 'choice', probabilities: { yes: 0.05, no: 0.95 } }, q1: { type: 'bool', probability: 0.1 } } }));
  const classify = api.models.classify;
  api.models.classify = ((_m: unknown, req: never) => ((sent = req), classify())) as never;
  const answered = await answerItems(api as never, { ref, memo: 'm', clef: limiter(1), escalations: limiter(1) }, [{ unit, checks: two }], BACKGROUND_CONTEXT);
  assert.deepEqual(Object.keys(sent!.questions), ['q0', 'q1']);
  assert.doesNotMatch(JSON.stringify(sent), /no_self_contradiction|says_why/);
  assert.deepEqual(answered.map((a) => [a.check.id, a.answer!.met]), [['c.no_self_contradiction', true], ['c.says_why', false]]);
});

test('a classifier refused with a 503 is asked again after the backoff and its answer stands', async () => {
  const outcomes = [{ stopReason: 'error', answers: {}, errorMessage: 'Typesafe system one (503): busy' }, confident];
  const api = callApi(() => Promise.resolve(outcomes.shift()));
  const slept: number[] = [];
  const retry = { sleep: async (ms: number) => void slept.push(ms), random: () => 0, log: () => {} };
  const [a] = await answerItems(api as never, { ref, memo: 'judge:github.review:v1', clef: limiter(1), escalations: limiter(1), retry }, [{ unit, checks }], BACKGROUND_CONTEXT);
  assert.deepEqual([api.calls(), slept, a.answer!.resolver], [2, [2000], 'clef']);
});

test('a request the classifier refuses as too large is over the cap: not retried, memoized, escalated', async () => {
  const over = { stopReason: 'error', answers: {}, errorMessage: 'System One API error (500): input (8288 tokens) is too large to process' };
  const api = callApi(() => Promise.resolve(over));
  const retry = { sleep: async () => assert.fail('an input too large is not retried'), random: () => 0, log: () => {} };
  const [a] = await answerItems(api as never, { ref, memo: 'judge:github.review:v1', clef: limiter(1), escalations: limiter(1), retry }, [{ unit, checks }], BACKGROUND_CONTEXT);
  assert.deepEqual([api.calls(), api.memos.get(memoKey), a.answer!.resolver], [1, null, 'agent-model']);
});

test('an over-cap unit is memoized: a rerun escalates without asking the classifier', async () => {
  const api = callApi(() => Promise.resolve(confident));
  const tiny = { ...ref, maxInputTokens: 1 };
  await answerUnit(api, tiny, BACKGROUND_CONTEXT);
  assert.equal(api.memos.get(memoKey), null);
  await answerUnit(api, ref, BACKGROUND_CONTEXT);
  assert.equal(api.calls(), 0, 'the memoized miss stands even once the unit would fit');
});

test('an abort propagates instead of escalating', async () => {
  const ac = new AbortController();
  const api = callApi(() => {
    ac.abort(new Error('closing'));
    return Promise.reject(new DOMException('aborted', 'AbortError'));
  });
  await assert.rejects(answerUnit(api, ref, withAbortSignal(ac.signal, BACKGROUND_CONTEXT)), /closing/);
  assert.equal(api.memos.has(memoKey), false);
});

test('units that share context pack in order up to the cap; one without it goes by itself, and those alone over the cap go together', () => {
  const shared = { guide: 'g'.repeat(300) };
  const item = (id: string, text: string, s: Record<string, string> | undefined = shared) => ({ unit: { id, state: { text }, ...(s ? { shared: s } : {}) }, checks });
  const items = [item('a', 'x'), item('b', 'y'), item('lone', 'z', undefined), item('c', 'w'.repeat(400)), item('huge', 'h'.repeat(3000))];
  const one = classifierTokens(items[0].unit, checks);
  const ids = (cap: number | undefined) => pack(items, cap).map((g) => g.map((it) => it.unit.id));
  assert.deepEqual(ids(Math.ceil(one * 1.5)), [['a', 'b'], ['lone'], ['c', 'huge']], 'c and huge each exceed the cap, so they escalate as one');
  assert.deepEqual(ids(undefined), [['a'], ['b'], ['lone'], ['c'], ['huge']], 'without a classifier nothing packs');
  assert.ok(classifierTokens([items[0].unit, items[1].unit], checks) < one * 1.5, 'the shared context is counted once');
});

test('a packed request sends the context once and one question per unit; its unsure units escalate together in one judge-model request', async () => {
  const shared = { intent: 'serve the page' };
  const units = ['u0', 'u1', 'u2', 'u3'].map((id) => ({ id, shared, state: { text: id } }));
  const sent: Array<{ state: unknown; questions: Record<string, { instructions: string }> }> = [];
  const probabilities = [0.95, 0.5, 0.6, 0.05];
  const api = callApi(async () => ({ stopReason: 'stop', answers: Object.fromEntries(probabilities.map((p, i) => [`q${i}`, { type: 'bool', probability: p }])) }));
  const classify = api.models.classify;
  api.models.classify = ((_m: unknown, req: never) => (sent.push(req), classify())) as never;
  const prompts: string[] = [];
  Object.assign(api, { now: () => 0, agent: async () => ({}) });
  Object.assign(api.models, {
    getModel: (provider: string, id: string) => ({ provider, id }),
    completeSimple: async (_m: unknown, r: { messages: Array<{ content: string }> }) => {
      prompts.push(r.messages[0].content);
      return { stopReason: 'stop', content: [{ type: 'text', text: '{"q0": {"choice": "yes", "confidence": 0.9, "reason": "says so"}, "q1": {"choice": "no", "confidence": 0.8, "reason": "only asserts"}}' }] };
    },
  });
  let active = 0;
  let peak = 0;
  const one = limiter(1);
  const escalations: typeof one = (task) => one(async () => {
    peak = Math.max(peak, ++active);
    try {
      return await task();
    } finally {
      active--;
    }
  });
  const answered = await answerItems(api as never, { ref, judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations }, units.map((u) => ({ unit: u, checks })), BACKGROUND_CONTEXT);
  assert.equal(sent.length, 1);
  assert.deepEqual(sent[0].state, { context: shared, items: units.map((u) => ({ id: u.id, text: u.id })) });
  assert.equal(sent[0].questions.q2.instructions, 'About items[2] (u2): Does it say why?');
  assert.doesNotMatch(JSON.stringify(sent[0]), /says_why/, 'no check id reaches the classifier, which would read it as part of the question');
  assert.deepEqual(answered.map((a) => [a.unit.id, a.answer!.met, a.answer!.resolver, a.answer!.note]), [['u0', true, 'clef', undefined], ['u1', true, 'judge-model', 'says so'], ['u2', false, 'judge-model', 'only asserts'], ['u3', false, 'clef', undefined]]);
  assert.equal(prompts.length, 1, 'one judge-model request for both unsure units');
  assert.equal(prompts[0].split('serve the page').length, 2, 'the shared context is sent once');
  assert.match(prompts[0], /- q0: About items\[0\] \(u1\): Does it say why\?\n  Options: yes, no\n- q1: About items\[1\] \(u2\)/);
  assert.equal(peak, 1, 'the escalation limit holds');
  assert.ok(api.memos.has('m:u0+4:classifier'), 'a packed request is memoized under its first unit and size');
});

test('a question the packed reply leaves unanswered escalates on its own; a rerun reuses the packed reply', async () => {
  const shared = { diff: 'd' };
  const units = ['p0', 'p1'].map((id) => ({ id, shared, state: { promise: id } }));
  const api = callApi(async () => ({ stopReason: 'stop', answers: {} }));
  let packedCalls = 0;
  Object.assign(api, { now: () => 0, agent: async () => ({}) });
  Object.assign(api.models, {
    getModel: (provider: string, id: string) => ({ provider, id }),
    completeSimple: async (_m: unknown, r: { messages: Array<{ content: string }> }) => {
      const alone = r.messages[0].content.startsWith('Decision:');
      packedCalls += alone ? 0 : 1;
      return { stopReason: 'stop', content: [{ type: 'text', text: alone ? '{"choice": "no", "confidence": 0.9, "reason": "missing"}' : '{"q0": {"choice": "yes", "confidence": 0.9, "reason": "in the diff"}}' }] };
    },
  });
  const settings = { ref, judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations: limiter(1) };
  for (let run = 0; run < 2; run++) {
    const answered = await answerItems(api as never, settings, units.map((u) => ({ unit: u, checks })), BACKGROUND_CONTEXT);
    assert.deepEqual(answered.map((a) => [a.unit.id, a.answer!.met, a.answer!.note]), [['p0', true, 'in the diff'], ['p1', false, 'missing']]);
  }
  assert.equal(packedCalls, 1);
});

test('a batch over the judge model window splits into sub-batches that each fit; a question a reply leaves out escalates alone', async () => {
  const shared = { intent: 'serve the page' };
  const units = ['u0', 'u1', 'u2', 'u3'].map((id) => ({ id, shared, state: { text: id + 'x'.repeat(1000) } }));
  const api = callApi(async () => ({ stopReason: 'stop', answers: Object.fromEntries(units.map((_, i) => [`c.says_why#${i}`, { type: 'bool', probability: 0.5 }])) }));
  const prompts: string[] = [];
  const window = 1400 - 400;
  Object.assign(api, { now: () => 0, agent: async () => ({}) });
  Object.assign(api.models, {
    getModel: (provider: string, id: string) => ({ provider, id, contextWindow: 1400, maxTokens: 400 }),
    completeSimple: async (_m: unknown, r: { messages: Array<{ content: string }> }) => {
      const text = r.messages[0].content;
      if (text.startsWith('Decision:')) return { stopReason: 'stop', content: [{ type: 'text', text: '{"choice": "no", "confidence": 0.9, "reason": "alone"}' }] };
      prompts.push(text);
      // The second sub-batch's reply leaves out its second question.
      const reply = prompts.length === 1 ? '{"q0": {"choice": "yes", "confidence": 0.9, "reason": "a"}, "q1": {"choice": "yes", "confidence": 0.9, "reason": "b"}}' : '{"q0": {"choice": "yes", "confidence": 0.9, "reason": "c"}}';
      return { stopReason: 'stop', content: [{ type: 'text', text: reply }] };
    },
  });
  let [active, peak] = [0, 0];
  const one = limiter(1);
  const escalations: typeof one = (task) => one(async () => {
    peak = Math.max(peak, ++active);
    try {
      return await task();
    } finally {
      active--;
    }
  });
  const answered = await answerItems(api as never, { ref, judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations }, units.map((u) => ({ unit: u, checks })), BACKGROUND_CONTEXT);
  assert.equal(prompts.length, 2, 'two sub-batches, each its own request');
  for (const p of prompts) {
    assert.ok(p.length / 3 <= window, 'each sub-batch fits the window');
    assert.equal(p.split('serve the page').length, 2, 'the shared context is sent once per sub-batch');
  }
  assert.deepEqual(answered.map((a) => [a.unit.id, a.answer!.met, a.answer!.note]), [['u0', true, 'a'], ['u1', true, 'b'], ['u2', true, 'c'], ['u3', false, 'alone']]);
  const key = (ids: string[]) => `m:escalation:${createHash('sha256').update(ids.map((id) => `${id}:c.says_why`).join('\n')).digest('hex').slice(0, 16)}`;
  assert.ok(api.memos.has(key(['u0', 'u1'])) && api.memos.has(key(['u2', 'u3'])), 'each sub-batch is memoized under its own questions');
  assert.deepEqual([...api.memos.keys()].filter((k) => k.startsWith('decision:m:')), ['decision:m:u3:c.says_why'], 'only the left-out question is asked alone');
  assert.equal(peak, 1, 'a left-out question waits for its own slot');
});

test('a check with classifier false never reaches the classifier and is escalated with the group\'s other unsure questions', async () => {
  const shared = { ci: 'ci=success' };
  const units = ['f0', 'f1'].map((id) => ({ id, shared, state: { finding: id } }));
  const contra = { id: 'c.contradicted', question: { type: 'bool' as const, instructions: 'Does the code contradict it?', pass: ['no' as const] }, classifier: false as const };
  const sent: Array<{ questions: Record<string, { instructions: string }> }> = [];
  // Clef is sure about f0's says_why and unsure about f1's.
  const api = callApi(async () => ({ stopReason: 'stop', answers: { q0: { type: 'bool', probability: 0.95 }, q1: { type: 'bool', probability: 0.5 } } }));
  const classify = api.models.classify;
  api.models.classify = ((_m: unknown, req: never) => (sent.push(req), classify())) as never;
  const prompts: string[] = [];
  Object.assign(api, { now: () => 0, agent: async () => ({}) });
  Object.assign(api.models, {
    getModel: (provider: string, id: string) => ({ provider, id }),
    completeSimple: async (_m: unknown, r: { messages: Array<{ content: string }> }) => {
      prompts.push(r.messages[0].content);
      const reply = { q0: { choice: 'no', confidence: 0.9, reason: 'consistent' }, q1: { choice: 'yes', confidence: 0.9, reason: 'says so' }, q2: { choice: 'yes', confidence: 0.9, reason: 'line 47 gives 0' } };
      return { stopReason: 'stop', content: [{ type: 'text', text: JSON.stringify(reply) }] };
    },
  });
  const settings = { ref, judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations: limiter(1) };
  const answered = await answerItems(api as never, settings, units.map((u) => ({ unit: u, checks: [...checks, contra] })), BACKGROUND_CONTEXT);
  assert.deepEqual(sent.map((r) => Object.values(r.questions).map((q) => q.instructions)), [['About items[0] (f0): Does it say why?', 'About items[1] (f1): Does it say why?']]);
  assert.equal(prompts.length, 1, 'both contradicted questions and the unsure says_why go in one judge-model request');
  assert.deepEqual(answered.map((a) => [a.unit.id, a.check.id, a.answer!.met, a.answer!.resolver]), [
    ['f0', 'c.says_why', true, 'clef'], ['f0', 'c.contradicted', true, 'judge-model'], ['f1', 'c.says_why', true, 'judge-model'], ['f1', 'c.contradicted', false, 'judge-model'],
  ]);
  const only = callApi(async () => assert.fail('a group of classifier-false checks sends the classifier nothing'));
  Object.assign(only, { now: () => 0, agent: async () => ({}) });
  Object.assign(only.models, { getModel: (provider: string, id: string) => ({ provider, id }), completeSimple: async () => ({ stopReason: 'stop', content: [{ type: 'text', text: '{"choice": "no", "confidence": 0.9, "reason": "fine"}' }] }) });
  await answerItems(only as never, settings, [{ unit: { id: 'lone', state: { finding: 'x' } }, checks: [contra] }], BACKGROUND_CONTEXT);
  assert.equal(only.calls(), 0);
});

test('a limiter runs at most n tasks at once and starts the next as one ends', async () => {
  const limit = limiter(2);
  const gates: Array<() => void> = [];
  let active = 0;
  let peak = 0;
  const task = () => limit(() => new Promise<void>((r) => { peak = Math.max(peak, ++active); gates.push(() => (active--, r())); }));
  const all = Promise.all([task(), task(), task(), task()]);
  await new Promise((r) => setImmediate(r));
  assert.equal(gates.length, 2, 'two started, two wait');
  while (gates.length) {
    gates.shift()!();
    await new Promise((r) => setImmediate(r));
  }
  await all;
  assert.equal(peak, 2);
});
