import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT, withAbortSignal } from '@earendil-works/chord/context';
import type { Context } from '@earendil-works/chord/context';
import { CONFIDENT_ONLY, PASS_ONLY } from '../src/declaration.ts';
import { limiter } from '../src/limit.ts';
import { answerItems, batchKey, decisionModelTokens, MAX_BATCH_QUESTIONS, pack, PROMPT_VERSION, type DecisionModelRef } from '../src/resolve.ts';
import { reuseKey } from '../src/reuse.ts';

const ref = { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 };
const unit = { id: 'f1', state: { finding: '- nit: rename' } };
const checks = [{ id: 'c.says_why', question: { type: 'bool' as const, instructions: 'Does it say why?' } }];
const memoKey = 'judge:github.review:v1:f1:classifier';
// One unit judged alone, answers by check id.
const answerUnit = async (api: unknown, r: DecisionModelRef, ctx: Context) =>
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

// One round of the unit under a counted escalation limiter and silent, instant retries; `escalated` counts judge-model slots taken.
function counted(api: unknown) {
  const count = { escalated: 0, log: [] as string[] };
  const one = limiter(1);
  const retry = { sleep: async () => {}, random: () => 0, log: (l: string) => void count.log.push(l) };
  const s = { ref, memo: 'judge:github.review:v1', clef: limiter(1), escalations: ((task) => (count.escalated++, one(task))) as typeof one, retry };
  return { count, round: async () => (await answerItems(api as never, s, [{ unit, checks }], BACKGROUND_CONTEXT))[0].answer! };
}

const transient = ['connect ECONNREFUSED: connection refused', 'socket hang up', 'Request timed out', '429 Too Many Requests', 'System One API error (503): busy', 'server_busy the engine stopped while this request waited; it was not sent'];

test('a transient decision-model failure is an outage: its items stay undecided without a judge-model call, and a later round with it back decides them', async () => {
  for (const errorMessage of transient) {
    const api = callApi(async () => (api.calls() > 10 ? confident : { stopReason: 'error', answers: {}, errorMessage }));
    const { count, round } = counted(api);
    assert.deepEqual([(await round()).met, count.escalated, api.memos.has(memoKey), api.calls()], [null, 0, false, 10], `${errorMessage}: retried, then left undecided and not memoized`);
    const back = await round();
    assert.deepEqual([back.met, back.resolver, count.escalated], [true, 'clef', 0], errorMessage);
  }
  const thrown = callApi(() => Promise.reject(new Error('fetch failed')));
  const { count, round } = counted(thrown);
  assert.deepEqual([(await round()).met, count.escalated, thrown.memos.has(memoKey)], [null, 0, false], 'a thrown call is an outage too');
});

test('a durable decision-model refusal is not an outage: asked once, memoized, logged and escalated, so judging still ends', async () => {
  const refusals = ['401 Unauthorized', '403 Forbidden: invalid api key', '400 Bad Request', '404 model not found', '422 Unprocessable Entity'].map((errorMessage) => ({ stopReason: 'error', answers: {}, errorMessage }));
  for (const reply of [...refusals, { stopReason: 'length', answers: {} }]) {
    const api = callApi(async () => reply);
    const { count, round } = counted(api);
    const a = await round();
    assert.deepEqual([a.met, a.resolver, count.escalated, api.memos.get(memoKey), api.calls()], [true, 'agent-model', 1, null, 1], JSON.stringify(reply));
    assert.match(count.log.join('\n'), /decision model clef\/clef refused/);
  }
});

test('during an outage a check that skips the decision model still escalates', async () => {
  const api = callApi(() => Promise.reject(new Error('503 busy')));
  const skip = { id: 'c.says_why', question: checks[0].question, decisionModel: false as const };
  const two = [{ ...checks[0], id: 'c.asked' }, skip];
  const answered = await answerItems(api as never, { ref, memo: 'judge:github.review:v1', clef: limiter(1), escalations: limiter(1) }, [{ unit, checks: two }], BACKGROUND_CONTEXT);
  assert.deepEqual(answered.map((a) => [a.check.id, a.answer!.met, a.answer!.resolver]), [['c.asked', null, 'none'], ['c.says_why', true, 'agent-model']]);
});

test('a unit\'s questions go to the decision model numbered, never under their check ids, and each answer comes back to its check', async () => {
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

test('a decision model refused with a 503 is asked again after the backoff and its answer stands', async () => {
  const outcomes = [{ stopReason: 'error', answers: {}, errorMessage: 'Typesafe system one (503): busy' }, confident];
  const api = callApi(() => Promise.resolve(outcomes.shift()));
  const slept: number[] = [];
  const retry = { sleep: async (ms: number) => void slept.push(ms), random: () => 0, log: () => {} };
  const [a] = await answerItems(api as never, { ref, memo: 'judge:github.review:v1', clef: limiter(1), escalations: limiter(1), retry }, [{ unit, checks }], BACKGROUND_CONTEXT);
  assert.deepEqual([api.calls(), slept, a.answer!.resolver], [2, [2000], 'clef']);
});

test('a request the decision model refuses as too large is over the cap: not retried, memoized, escalated', async () => {
  const over = { stopReason: 'error', answers: {}, errorMessage: 'System One API error (500): input (8288 tokens) is too large to process' };
  const api = callApi(() => Promise.resolve(over));
  const retry = { sleep: async () => assert.fail('an input too large is not retried'), random: () => 0, log: () => {} };
  const [a] = await answerItems(api as never, { ref, memo: 'judge:github.review:v1', clef: limiter(1), escalations: limiter(1), retry }, [{ unit, checks }], BACKGROUND_CONTEXT);
  assert.deepEqual([api.calls(), api.memos.get(memoKey), a.answer!.resolver], [1, null, 'agent-model']);
});

test('an over-cap unit is memoized: a rerun escalates without asking the decision model', async () => {
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
  const item = (id: string, text: string, s: Record<string, string> | null = shared) => ({ unit: { id, state: { text }, ...(s ? { shared: s } : {}) }, checks });
  const items = [item('a', 'x'), item('b', 'y'), item('lone', 'z', null), item('c', 'w'.repeat(400)), item('huge', 'h'.repeat(3000))];
  const one = decisionModelTokens(items[0].unit, checks);
  const ids = (cap: number | undefined) => pack(items, cap).map((g) => g.map((it) => it.unit.id));
  assert.deepEqual(ids(Math.ceil(one * 1.5)), [['a', 'b'], ['lone'], ['c', 'huge']], 'c and huge each exceed the cap, so they escalate as one');
  const mixed = [item('over', 'w'.repeat(400)), item('small', 'x')];
  assert.deepEqual(pack(mixed, Math.ceil(one * 1.5)).map((g) => g.map((it) => it.unit.id)), [['over'], ['small']], 'an over-cap unit no longer drags the group\'s fitting units into its escalation batch');
  assert.deepEqual(ids(undefined), [['a', 'b', 'c', 'huge'], ['lone']], 'without a decision model, shared-context units still pack to escalate together');
  const noClef = items.map((it) => ({ ...it, checks: it.checks.map((c) => ({ ...c, decisionModel: false as const })) }));
  assert.deepEqual(pack(noClef, Math.ceil(one * 1.5)).map((g) => g.map((it) => it.unit.id)), [['a', 'b', 'c', 'huge'], ['lone']], 'decisionModel-false items are not split by the decision model cap');
  assert.ok(decisionModelTokens([items[0].unit, items[1].unit], checks) < one * 1.5, 'the shared context is counted once');
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
  assert.deepEqual(sent[0].state, { context: shared, asks: ['Does it say why?'], items: units.map((u) => ({ id: u.id, text: u.id })) });
  assert.equal(sent[0].questions.q2.instructions, 'About items[2]: answer asks[0].');
  assert.doesNotMatch(JSON.stringify(sent[0]), /says_why/, 'no check id reaches the decision model, which would read it as part of the question');
  assert.deepEqual(answered.map((a) => [a.unit.id, a.answer!.met, a.answer!.resolver, a.answer!.note]), [['u0', true, 'clef', undefined], ['u1', true, 'j', 'says so'], ['u2', false, 'j', 'only asserts'], ['u3', false, 'clef', undefined]]);
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

test('eight unsure questions of one group go to the judge model as two requests of four, side by side', async () => {
  const shared = { diff: 'd' };
  const units = Array.from({ length: 8 }, (_, n) => ({ id: `p${n}`, shared, state: { part: `p${n}` } }));
  const api = callApi(async () => ({ stopReason: 'stop', answers: {} }));
  const prompts: string[] = [];
  let [active, peak] = [0, 0];
  Object.assign(api, { now: () => 0, agent: async () => ({}) });
  Object.assign(api.models, {
    getModel: (provider: string, id: string) => ({ provider, id }),
    completeSimple: async (_m: unknown, r: { messages: Array<{ content: string }> }) => {
      prompts.push(r.messages[0].content);
      peak = Math.max(peak, ++active);
      await new Promise((done) => setImmediate(done));
      active--;
      const ids = [...r.messages[0].content.matchAll(/^- (q\d+):/gm)].map((m) => m[1]);
      return { stopReason: 'stop', content: [{ type: 'text', text: JSON.stringify(Object.fromEntries(ids.map((q) => [q, { choice: 'yes', confidence: 0.9, reason: 'kept' }]))) }] };
    },
  });
  const settings = { ref, judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations: limiter(2) };
  const answered = await answerItems(api as never, settings, units.map((u) => ({ unit: u, checks })), BACKGROUND_CONTEXT);
  assert.deepEqual(prompts.map((p) => (p.match(/^- q\d+:/gm) ?? []).length), [MAX_BATCH_QUESTIONS, MAX_BATCH_QUESTIONS]);
  assert.equal(peak, 2, 'the two sub-batches run in parallel within the escalation limit');
  assert.ok(answered.every((a) => a.answer!.met === true && a.answer!.resolver === 'j'));
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

test('a check with decisionModel false never reaches the decision model and is escalated with the group\'s other unsure questions', async () => {
  const shared = { ci: 'ci=success' };
  const units = ['f0', 'f1'].map((id) => ({ id, shared, state: { finding: id } }));
  const contra = { id: 'c.contradicted', question: { type: 'bool' as const, instructions: 'Does the code contradict it?', pass: ['no' as const] }, decisionModel: false as const };
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
  assert.deepEqual(sent.map((r) => Object.values(r.questions).map((q) => q.instructions)), [['About items[0]: answer asks[0].', 'About items[1]: answer asks[0].']]);
  assert.equal(prompts.length, 1, 'both contradicted questions and the unsure says_why go in one judge-model request');
  assert.deepEqual(answered.map((a) => [a.unit.id, a.check.id, a.answer!.met, a.answer!.resolver]), [
    ['f0', 'c.says_why', true, 'clef'], ['f0', 'c.contradicted', true, 'j'], ['f1', 'c.says_why', true, 'j'], ['f1', 'c.contradicted', false, 'j'],
  ]);
  const only = callApi(async () => assert.fail('a group of decisionModel-false checks sends the decision model nothing'));
  Object.assign(only, { now: () => 0, agent: async () => ({}) });
  Object.assign(only.models, { getModel: (provider: string, id: string) => ({ provider, id }), completeSimple: async () => ({ stopReason: 'stop', content: [{ type: 'text', text: '{"choice": "no", "confidence": 0.9, "reason": "fine"}' }] }) });
  await answerItems(only as never, settings, [{ unit: { id: 'lone', state: { finding: 'x' } }, checks: [contra] }], BACKGROUND_CONTEXT);
  assert.equal(only.calls(), 0);
});

// A pass-only pitfall: the item holds on "no", and only a confident "no" from the decision model stands.
const contradicts = { id: 'c.self', question: { type: 'choice' as const, instructions: 'Does any figure conflict?', options: { yes: 'Two passages differ.', no: 'They agree.' } }, pass: ['no'], decisionModel: PASS_ONLY };

// An api whose decision model answers each packed question with `noMass` (the probability of "no") and whose judge model answers
// every question it is sent with `choice`, counting requests and the most in flight at once.
function passOnlyApi(noMass: number[], choice: string) {
  const api = callApi(async () => ({ stopReason: 'stop', answers: Object.fromEntries(noMass.map((p, i) => [`q${i}`, { type: 'choice', probabilities: { yes: 1 - p, no: p } }])) }));
  const seen = { prompts: [] as string[], active: 0, peak: 0 };
  Object.assign(api, { now: () => 0, agent: async () => ({}) });
  Object.assign(api.models, {
    getModel: (provider: string, id: string) => ({ provider, id }),
    completeSimple: async (_m: unknown, r: { messages: Array<{ content: string }> }) => {
      const prompt = r.messages[0].content;
      seen.prompts.push(prompt);
      seen.peak = Math.max(seen.peak, ++seen.active);
      await new Promise((res) => setImmediate(res));
      seen.active--;
      const n = (prompt.match(/^- q\d+:/gm) ?? []).length;
      const one = { choice, confidence: 0.9, reason: `the judge says ${choice}` };
      const text = n ? JSON.stringify(Object.fromEntries(Array.from({ length: n }, (_, i) => [`q${i}`, one]))) : JSON.stringify(one);
      return { stopReason: 'stop', content: [{ type: 'text', text }] };
    },
  });
  return { api, seen };
}

test('a pass-only check keeps a confident pass from the decision model; a confident fail and an unsure answer go to the judge model, whose answer and reason stand', async () => {
  const shared = { request: 'r' };
  const units = ['u0', 'u1', 'u2'].map((id) => ({ id, shared, state: { report: id } }));
  const { api, seen } = passOnlyApi([0.95, 0.03, 0.5], 'no');
  const settings = { ref, judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations: limiter(1) };
  const answered = await answerItems(api as never, settings, units.map((u) => ({ unit: u, checks: [contradicts] })), BACKGROUND_CONTEXT);
  assert.deepEqual(answered.map((a) => [a.unit.id, a.answer!.met, a.answer!.resolver, a.answer!.note]), [
    ['u0', true, 'clef', undefined],
    ['u1', true, 'j', 'the judge says no'],
    ['u2', true, 'j', 'the judge says no'],
  ]);
  assert.equal(seen.prompts.length, 1, 'the two escalations go in one judge-model request');
});

test('a confident fail stands on a check without the option and escalates on a pass-only one, asked of the same unit', async () => {
  const plain = { ...contradicts, id: 'c.plain', decisionModel: undefined };
  const { api, seen } = passOnlyApi([0.02, 0.02], 'yes');
  const settings = { ref, judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations: limiter(1) };
  const answered = await answerItems(api as never, settings, [{ unit: { id: 'r', state: { report: 'x' } }, checks: [plain, contradicts] }], BACKGROUND_CONTEXT);
  assert.deepEqual(answered.map((a) => [a.check.id, a.answer!.met, a.answer!.resolver, a.answer!.note]), [
    ['c.plain', false, 'clef', undefined],
    ['c.self', false, 'j', 'the judge says yes'],
  ]);
  assert.equal(seen.prompts.length, 1);
});

test('a confident-only check keeps the decision model\'s confident pass and fail, and an unsure item holds without a judge-model call', async () => {
  const style = { ...contradicts, id: 'c.style', decisionModel: CONFIDENT_ONLY };
  const units = ['u0', 'u1', 'u2'].map((id) => ({ id, shared: { request: 'r' }, state: { report: id } }));
  const { api, seen } = passOnlyApi([0.95, 0.03, 0.5], 'yes');
  const settings = { ref, judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations: limiter(1) };
  const answered = await answerItems(api as never, settings, units.map((u) => ({ unit: u, checks: [style] })), BACKGROUND_CONTEXT);
  assert.deepEqual(answered.map((a) => [a.unit.id, a.answer!.met, a.answer!.resolver, a.answer!.cause]), [
    ['u0', true, 'clef', undefined],
    ['u1', false, 'clef', undefined],
    ['u2', true, 'clef', undefined],
  ]);
  assert.equal(seen.prompts.length, 0);
});

test('many confident fails on a pass-only check escalate in sub-batches within the escalation limit', async () => {
  const shared = { request: 'r' };
  const units = Array.from({ length: 9 }, (_, i) => ({ id: `u${i}`, shared, state: { report: `u${i}` } }));
  const { api, seen } = passOnlyApi(units.map(() => 0.01), 'yes');
  const settings = { ref, judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations: limiter(2) };
  const answered = await answerItems(api as never, settings, units.map((u) => ({ unit: u, checks: [contradicts] })), BACKGROUND_CONTEXT);
  assert.ok(answered.every((a) => a.answer!.resolver === 'j' && a.answer!.met === false));
  assert.equal(seen.prompts.length, Math.ceil(units.length / MAX_BATCH_QUESTIONS));
  assert.ok(seen.peak <= 2, `at most two judge-model requests at once, saw ${seen.peak}`);
});

test('a pass-only item is never failed on the decision model alone: with no judge model answer it stays undecided', async () => {
  const { api } = passOnlyApi([0.01], 'yes');
  Object.assign(api.models, { getModel: () => undefined });
  const settings = { ref, judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations: limiter(1) };
  const [a] = await answerItems(api as never, settings, [{ unit: { id: 'r', state: { report: 'x' } }, checks: [contradicts] }], BACKGROUND_CONTEXT);
  assert.deepEqual([a.answer!.met, a.answer!.resolver], [null, 'none']);
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

test('a transient askBatch error is not memoized: a retry calls the judge model again', async () => {
  const shared = { diff: 'd' };
  const units = ['p0', 'p1'].map((id) => ({ id, shared, state: { promise: id } }));
  const api = callApi(async () => ({ stopReason: 'stop', answers: {} }));
  let batchCalls = 0;
  let fail = true;
  Object.assign(api, { now: () => 0, agent: async () => ({}) });
  Object.assign(api.models, {
    getModel: (provider: string, id: string) => ({ provider, id }),
    completeSimple: async (_m: unknown, r: { messages: Array<{ content: string }> }) => {
      const alone = r.messages[0].content.startsWith('Decision:');
      if (!alone) batchCalls++;
      if (fail) return { stopReason: 'error', errorMessage: '503 busy', content: [] };
      return { stopReason: 'stop', content: [{ type: 'text', text: '{"q0": {"choice": "yes", "confidence": 0.9, "reason": "ok"}, "q1": {"choice": "yes", "confidence": 0.9, "reason": "ok"}}' }] };
    },
  });
  const retry = { sleep: async () => {}, random: () => 0, log: () => {} };
  const settings = { judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations: limiter(1), retry };
  const pending = units.map((unit, i) => ({ i, unit, check: checks[0] }));
  const key = batchKey('m', pending);

  const first = await answerItems(api as never, settings, units.map((u) => ({ unit: u, checks })), BACKGROUND_CONTEXT);
  assert.equal(api.memos.has(key), false, 'a transient outage is not memoized');
  assert.ok(first.every((a) => a.answer!.met === null));

  fail = false;
  const second = await answerItems(api as never, settings, units.map((u) => ({ unit: u, checks })), BACKGROUND_CONTEXT);
  assert.ok(batchCalls > 1, 'the retry calls the judge model again in a batch');
  assert.equal(api.memos.has(key), true);
  assert.deepEqual(second.map((a) => [a.unit.id, a.answer!.met, a.answer!.resolver]), [['p0', true, 'j'], ['p1', true, 'j']]);
});

test('answerItems skips the decision model when need() is already false for every item in the group', async () => {
  const api = callApi(async () => assert.fail('classify must not be called when every check is already decided'));
  const seen: unknown[] = [];
  const answered = await answerItems(api as never, { ref, memo: 'm', clef: limiter(1), escalations: limiter(1) }, [{ unit, checks }], BACKGROUND_CONTEXT, async (a) => void seen.push(a), () => false);
  assert.equal(api.calls(), 0);
  assert.equal(api.memos.has('m:f1:classifier'), false);
  assert.deepEqual(answered, [{ unit, check: checks[0], answer: undefined }]);
  assert.deepEqual(seen, answered);
});

// Reads a packed request as the decision model would: each question's item by index, its text inline or from `asks`.
function readRequest(req: { state: { asks?: string[]; items: Array<{ id: string; text: string }> }; questions: Record<string, { instructions: string }> }) {
  return Object.fromEntries(Object.entries(req.questions).map(([q, { instructions }]) => {
    const [, i, rest] = /^About items\[(\d+)\]: (.*)$/s.exec(instructions)!;
    const ask = /^answer asks\[(\d+)\]\.$/.exec(rest);
    return [q, `${req.state.items[Number(i)].text} | ${ask ? req.state.asks![Number(ask[1])] : rest}`];
  }));
}

test('a packed request states a question asked of several items once and maps every answer back to its item and check', async () => {
  const shared = { part: 'the document part' };
  const [a, b] = [{ id: 'c.a', question: { type: 'bool' as const, instructions: 'Does the part make it hold?' } }, { id: 'c.b', question: { type: 'bool' as const, instructions: 'Is it named?' } }];
  const items = [{ unit: { id: 'guarantee one ~ part', shared, state: { text: 'one' } }, checks: [a, b] }, ...['two', 'three'].map((t) => ({ unit: { id: `guarantee ${t} ~ part`, shared, state: { text: t } }, checks: [a] }))];
  // The decision model passes exactly the pairs below, read from the request alone.
  const holds = new Set(['one | Does the part make it hold?', 'three | Does the part make it hold?', 'two | Is it named?']);
  let sent: any;
  const api = callApi(async () => ({ stopReason: 'stop', answers: Object.fromEntries(Object.entries(readRequest(sent)).map(([q, read]) => [q, { type: 'bool', probability: holds.has(read) ? 0.95 : 0.05 }])) }));
  const classify = api.models.classify;
  api.models.classify = ((_m: unknown, req: never) => ((sent = req), classify())) as never;
  const answered = await answerItems(api as never, { ref, memo: 'm', clef: limiter(1), escalations: limiter(1) }, items, BACKGROUND_CONTEXT);
  assert.deepEqual(sent.state.asks, ['Does the part make it hold?'], 'asked of three items: stated once');
  assert.equal(sent.questions.q1.instructions, 'About items[0]: Is it named?', 'asked of one item: inline');
  assert.doesNotMatch(JSON.stringify(sent), /c\.a|c\.b/, 'no check id reaches the decision model');
  assert.equal(JSON.stringify(sent).split('guarantee two ~ part').length, 2, 'a unit id is sent once, with its item');
  assert.deepEqual(answered.map((x) => [x.unit.state.text, x.check.id, x.answer!.met]), [['one', 'c.a', true], ['one', 'c.b', false], ['two', 'c.a', false], ['three', 'c.a', true]]);
});

test('a question goes to asks from its second item in a request, and the smaller request fits more items under the cap', () => {
  const shared = { part: 'p' };
  const long = { id: 'c.long', question: { type: 'bool' as const, instructions: `Does it hold? ${'x'.repeat(600)}` } };
  const units = ['a', 'b', 'c'].map((id) => ({ id: `scope ${id}`, shared, state: { text: id } }));
  assert.equal(decisionModelTokens([units[0], units[1]], [long]) - decisionModelTokens(units[0], [long]) < 200 / 3, true, 'the second item adds its reference, not the question');
  const cap = Math.ceil(decisionModelTokens(units, [long]));
  assert.deepEqual(pack(units.map((unit) => ({ unit, checks: [long] })), cap).map((g) => g.length), [3], 'three items with a 600-byte question fit one request at its own size');
});

test('the reuse key is computed as before; only the prompt version moved', () => {
  const ask = { id: 'c.kept', question: { type: 'bool' as const, instructions: 'Does it keep the rule?' } };
  const unit = { id: 'rows a', state: { row: { id: 'a' } }, shared: { guide: 'g' } };
  const policy = { decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } };
  // The key main computed at prompt version 1 for this item.
  assert.equal(reuseKey(ask, unit, policy, 1), 'c327464a2db21900b572b113a2bab98ff837f5b6a0c1bfd1bfac0632e7659528');
  assert.equal(PROMPT_VERSION, 3);
  assert.notEqual(reuseKey(ask, unit, policy), reuseKey(ask, unit, policy, 1));
});

test('at escalations: 1, a deciding answer is counted before the next waiter takes the slot', async () => {
  const units = ['u0', 'u1', 'u2'].map((id) => ({ id, state: { item: id } }));
  let escalated = 0;
  const memos = new Map<string, unknown>();
  const api = {
    now: () => 0,
    agent: async () => ({ model: { provider: 'local', modelId: 'j' } }),
    async memo(name: string, ...rest: unknown[]) {
      if (rest.length === 2 && !memos.has(name)) memos.set(name, rest[0]);
      return memos.get(name);
    },
    models: {
      getAllModels: () => [],
      getModel: () => ({ id: 'j', provider: 'local', contextWindow: 8192 }),
      completeSimple: async () => {
        escalated++;
        await Promise.resolve();
        return { stopReason: 'stop', content: [{ type: 'text', text: '{"choice":"yes","confidence":1,"reason":"holds"}' }] };
      },
    },
  };
  let decided = false;
  const settings = { judgeModel: { provider: 'local', modelId: 'j' }, memo: 'm', clef: limiter(1), escalations: limiter(1) };
  const answered = await answerItems(
    api as never,
    settings,
    units.map((u) => ({ unit: u, checks })),
    BACKGROUND_CONTEXT,
    async (a) => { if (a.answer?.met) decided = true; },
    () => !decided,
  );
  assert.equal(escalated, 1, 'no extra item escalates once the first deciding pass is counted');
  assert.deepEqual(answered.map((a) => a.answer?.met), [true, undefined, undefined]);
});

test('each item that leaves the decision model carries its cause, and one Clef settled carries none', async () => {
  const causeOf = async (api: unknown, r: DecisionModelRef, ask: Record<string, unknown> = {}) => (await answerItems(api as never, { ref: r, memo: 'judge:github.review:v1', clef: limiter(1), escalations: limiter(1), retry: { sleep: async () => {}, random: () => 0, log: () => {} } }, [{ unit, checks: [{ ...checks[0], ...ask }] }], BACKGROUND_CONTEXT))[0].answer!.cause;
  const reply = (probability: number) => callApi(async () => ({ stopReason: 'stop', answers: { q0: { type: 'bool', probability } } }));
  assert.equal(await causeOf(reply(0.95), ref), undefined);
  assert.equal(await causeOf(reply(0.5), ref), 'unsure');
  assert.equal(await causeOf(reply(0.05), ref, { decisionModel: PASS_ONLY }), 'pass-only-no');
  assert.equal(await causeOf(reply(0.95), ref, { decisionModel: false }), 'declared');
  assert.equal(await causeOf(reply(0.95), { ...ref, maxInputTokens: 1 }), 'over-window');
  assert.equal(await causeOf(reply(0.95), { ...ref, maxInputTokens: 1 }, { decisionModel: CONFIDENT_ONLY }), 'over-window', 'a confident-only item Clef never answered still escalates');
  assert.equal(await causeOf(callApi(async () => ({ stopReason: 'error', answers: {}, errorMessage: '400 Bad Request' })), ref), 'refused');
  assert.equal(await causeOf(callApi(() => Promise.reject(new Error('503 busy'))), ref), 'outage');
});

test('with no decision model an escalated item records no cause', async () => {
  const api = callApi(async () => confident);
  const [a] = await answerItems(api as never, { memo: 'judge:github.review:v1', clef: limiter(1), escalations: limiter(1) }, [{ unit, checks }], BACKGROUND_CONTEXT);
  assert.equal(a.answer!.cause, undefined);
  assert.equal(a.answer!.resolver, 'agent-model');
});
