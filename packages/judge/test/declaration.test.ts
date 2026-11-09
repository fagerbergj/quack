import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { buildEvidence, type Declaration, type ModelSplit } from '../src/declaration.ts';
import { fold, metOf } from '../src/fold.ts';
import { registerSplitter, rejudge, type Inputs, type JudgeOptions, type Rubric } from '../src/index.ts';

const T = true;
const F = false;
const U = undefined;

test('the fold decides as soon as no assignment of the undecided items can change it', () => {
  assert.equal(fold({ name: 'all' }, [T, T, U]), U, 'all: one undecided item could still fail it');
  assert.equal(fold({ name: 'all' }, [F, U, U]), F, 'all: one failing item decides it');
  assert.equal(fold({ name: 'all' }, [T, T]), T);
  assert.equal(fold({ name: 'any' }, [F, U, T]), T, 'any: one holding item decides it');
  assert.equal(fold({ name: 'any' }, [F, U]), U);
  assert.equal(fold({ name: 'any' }, [F, F]), F);
  assert.equal(fold({ name: 'share', min: 0.5 }, [T, T, U, U]), T, 'share: half already holds');
  assert.equal(fold({ name: 'share', min: 0.75 }, [T, F, F, U]), F, 'share: three quarters can no longer hold');
  assert.equal(fold({ name: 'share', min: 0.75 }, [T, T, F, U]), U);
  assert.equal(fold({ name: 'atLeast', k: 2 }, [T, U, T]), T);
  assert.equal(fold({ name: 'atLeast', k: 2 }, [T, F, F]), F);
  assert.equal(fold({ name: 'coverageUnion', min: 0.5 }, [T, F, U, U]), U);
});

test('met: a positive check holds when the fold is true, a pitfall is present when it is false; share and atLeast report the share', () => {
  assert.equal(metOf({ name: 'all' }, true, [T, T], false), true);
  assert.equal(metOf({ name: 'all' }, false, [T, F], true), true, 'a failing item makes the pitfall present');
  assert.equal(metOf({ name: 'any' }, true, [T, F], true), false);
  assert.equal(metOf({ name: 'share', min: 0.5 }, true, [T, T, F, U], false), 0.5);
  assert.equal(metOf({ name: 'share', min: 0.5 }, true, [T, T, F, U], true), 0.25, "a pitfall's share is of the items where the claim failed");
});

const report = '# Title\nIntro.\n## Findings\n- Buses cost $2 million each. [s1]\n- The fleet has 38 buses.\n- ok\n## Notes\n- skip me please now\n';
const cards = JSON.stringify({ rows: [{ id: 'r1', player: 'p1', action: 'start' }, { id: 'r2', player: 'p2', action: 'keep' }, { id: 'r3', player: 'p3', action: 'sit' }] });
const facts = JSON.stringify({ p1: { injured: true }, p2: { injured: false }, p3: { injured: false } });
const inputs: Inputs = { report, cards, facts, intent: JSON.stringify({ purpose: 'judge', guarantees: ['g'] }), files: 'diff --git a/src/a.ts b/src/a.ts\n@@ -1 +1 @@\n-x\n+y\ndiff --git a/src/b.ts b/src/b.ts\nnew file mode 100644\n@@ -0,0 +1 @@\n+z\n' };

test('a scope builds one unit per item: the piece, item context in state, context every item sees identically in shared', async () => {
  const decl: Declaration = {
    scopes: {
      rows: {
        split: { name: 'jsonItems', from: 'cards', path: 'rows', where: [{ path: 'action', op: 'ne', value: 'sit' }] },
        piece: 'row',
        context: { intent: { name: 'json', from: 'intent', pointer: '/purpose' }, facts: { name: 'join', from: 'facts', by: 'player' } },
      },
      whole: { split: { name: 'whole', from: 'intent' }, piece: 'intent' },
    },
    checks: {},
  };
  const { evidence, splits } = await buildEvidence(decl, inputs);
  assert.deepEqual(evidence.rows.map((u) => u.id), ['rows r1', 'rows r2'], 'ids are scope-qualified and the where dropped the sit row');
  assert.deepEqual(evidence.rows[0], { id: 'rows r1', state: { row: { id: 'r1', player: 'p1', action: 'start' }, facts: { injured: true } }, related: { facts: 1 }, shared: { intent: 'judge' } });
  assert.deepEqual(evidence.whole[0].id, 'whole intent');
  assert.deepEqual(evidence.whole[0].state, { intent: { purpose: 'judge', guarantees: ['g'] } }, 'JSON text goes to the model as JSON');
  assert.deepEqual(splits.rows, { splitter: 'jsonItems' });
});

test('a cross-input where compares an item field against its joined record; related needs a match when it requires one', async () => {
  const decl: Declaration = {
    scopes: {
      injured: { split: { name: 'jsonItems', from: 'cards', path: 'rows', where: [{ path: 'action', op: 'ne', as: 'facts', field: 'injured' }] }, context: { facts: { name: 'join', from: 'facts', by: 'player' } } },
      cited: { split: { name: 'listItems', from: 'report', sections: ['Findings'], strip: '\\[s\\d+\\]', minWords: 3 }, context: { file: { name: 'related', chunker: { name: 'diffFiles', from: 'files' }, match: ['names', 'overlap'], require: true } } },
    },
    checks: {},
  };
  const { evidence } = await buildEvidence(decl, { ...inputs, report: '## Findings\n- src/a.ts swaps x for y [s1]\n- nothing about files here\n- ok\n' });
  assert.deepEqual(evidence.injured.map((u) => u.id), ['injured r1', 'injured r2', 'injured r3'], 'an action is never equal to an injury flag');
  assert.deepEqual(evidence.cited.map((u) => [u.id, u.state.piece, u.related]), [['cited item-1', 'src/a.ts swaps x for y', { file: 1 }]], 'the item naming no file is dropped; the short one never split');
  assert.match(String(evidence.cited[0].state.file), /^src\/a\.ts: diff --git a\/src\/a\.ts/, 'a candidate is shown under its key');
});

test('text splitters read sections, strip, match and cap, and a declared cut is marked in the context', async () => {
  const decl: Declaration = {
    scopes: {
      claims: { split: { name: 'sentences', from: 'report', skip: ['Notes'], match: '\\d', max: 1 }, context: { notes: { name: 'sections', from: 'report', names: ['Notes'], maxChars: 6 } } },
      sections: { split: { name: 'markdownSections', from: 'report', names: ['notes'] } },
    },
    checks: {},
  };
  const { evidence } = await buildEvidence(decl, inputs);
  assert.deepEqual(evidence.claims.map((u) => u.state.piece), ['Buses cost $2 million each. [s1]']);
  assert.equal(evidence.claims[0].shared!.notes, '- skip\n… (cut at 6 characters)');
  assert.deepEqual(evidence.sections.map((u) => u.id), ['sections Notes']);
});

test('map-reduce pairs passages whose digests conflict', async () => {
  const text = '- The fleet has 38 electric buses in service.\n- The fleet has 42 electric buses in service.\n- Tickets cost $2.\n';
  const decl: Declaration = { scopes: { pairs: { split: { name: 'digestPairs', from: 'x', of: { name: 'listItems', from: 'x' }, extract: 'numbers', compare: 'sameTermsOtherValue' } } }, checks: {} };
  const { evidence } = await buildEvidence(decl, { x: text });
  assert.deepEqual(evidence.pairs.map((u) => [u.id, u.state.piece]), [['pairs item-1|item-2', { first: 'The fleet has 38 electric buses in service.', second: 'The fleet has 42 electric buses in service.' }]]);
});

test('an off-shape input falls back to the llm splitter, whose made-up spans are dropped and whose sweep judges what it missed', async () => {
  const calls: string[] = [];
  const model: ModelSplit = async (text, _o, role) => {
    calls.push(role);
    return [{ item: 'buses cost two million', span: 'Buses cost $2   million each.' }, { item: 'invented', span: 'not in the text' }];
  };
  const llm = { name: 'llm', from: 'report', sections: ['Findings'], sweep: { name: 'listItems', from: 'report', strip: '\\[s\\d+\\]' } };
  const decl: Declaration = { scopes: { asks: { split: { name: 'listItems', from: 'report', sections: ['Findings'], expect: { min: 5 }, fallback: llm } } }, checks: {} };
  const { evidence, splits } = await buildEvidence(decl, inputs, model);
  assert.deepEqual(splits.asks, { splitter: 'listItems -> llm', reason: 'fewer than 5 items' });
  assert.deepEqual(calls, ['asks:fallback']);
  assert.deepEqual(evidence.asks.map((u) => [u.id, u.state.piece]), [['asks i1', 'buses cost two million'], ['asks xitem-2', 'The fleet has 38 buses.'], ['asks xitem-3', 'ok']]);
  const failed = await buildEvidence({ scopes: { asks: { split: llm } }, checks: {} }, inputs, async () => undefined);
  assert.deepEqual([failed.evidence.asks, failed.splits.asks.failed], [[], 'the splitting model gave no usable split']);
});

test('a declaration error is loud: an input nobody provided, a splitter nobody registered, a second strategy under one name', async () => {
  await assert.rejects(buildEvidence({ scopes: { s: { split: { name: 'whole', from: 'nope' } } }, checks: {} }, inputs), /reads input "nope"/);
  await assert.rejects(buildEvidence({ scopes: { s: { split: { name: 'nope', from: 'report' } } }, checks: {} }, inputs), /no splitter "nope"/);
  const mine = () => [];
  registerSplitter('test.mine', mine);
  registerSplitter('test.mine', mine);
  assert.throws(() => registerSplitter('test.mine', () => []), /already registered/);
  assert.throws(() => registerSplitter('whole', () => []), /library's own/);
});

// A tool call's api as the judge reads it: memos, a classifier and a judge model, with every request recorded.
function fakeApi(clef: (q: string) => unknown, judge: (name: string) => string) {
  const memos = new Map<string, unknown>();
  const requests: Array<{ state: unknown; questions: Record<string, unknown> }> = [];
  const escalated: string[] = [];
  const api = {
    conversationId: 'c',
    now: () => 0,
    async memo(name: string, ...rest: unknown[]) {
      if (rest.length === 2 && !memos.has(name)) memos.set(name, rest[0]);
      return memos.get(name);
    },
    snapshot: async () => undefined,
    agent: async () => ({}),
    models: {
      getAllModels: () => [{ id: 'clef', type: 'classifier' }],
      classify: async (_m: unknown, req: { state: unknown; questions: Record<string, unknown> }) => (requests.push(req), { stopReason: 'stop', answers: Object.fromEntries(Object.keys(req.questions).map((q) => [q, clef(q)])) }),
      getModel: (provider: string, id: string) => ({ provider, id }),
      completeSimple: async (_m: unknown, r: { messages: Array<{ content: string }> }) => {
        const name = /^Decision: ([\s\S]*?)\n/.exec(r.messages[0].content)?.[1] ?? 'split';
        escalated.push(name);
        return { stopReason: 'stop', content: [{ type: 'text', text: judge(name) }] };
      },
    },
  };
  return { api: api as never, requests, escalated };
}

const sealed = { kind: 'k', version: 1, hash: '', content: 'x', sealedAt: 0, verdict: null };
const rows = JSON.stringify([{ id: 'a' }, { id: 'b' }, { id: 'c' }]);
const opts = (rubric: Rubric, declaration: Declaration, more: Inputs = {}): JudgeOptions => ({
  kind: { kind: 'k', path: 'x' }, rubric, declaration, inputs: async () => ({ rows, guide: '{"rule":"r"}', ...more }), maxRevisions: 0,
  classifier: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 }, judgeModel: { provider: 'judge', modelId: 'j' },
});
const yes = JSON.stringify({ choice: 'yes', confidence: 0.9, reason: 'fine' });
const rubricOf = (id: string, weight: number | 'gate'): Rubric => ({ name: 'r', version: 1, threshold: 0.8, criteria: [{ id: 'c', checks: [{ id, label: id, weight }] }] });
const scope = { split: { name: 'jsonItems', from: 'rows' }, piece: 'row', context: { guide: { name: 'json', from: 'guide' } } };

test('a pitfall decided by one failing item skips the escalation the other unsure item would have needed, and lists it', async () => {
  // Clef: a fails the claim outright, b is unsure, c holds.
  const p: Record<string, number> = { 'c.kept#0': 0.05, 'c.kept#1': 0.5, 'c.kept#2': 0.95 };
  const { api, requests, escalated } = fakeApi((q) => ({ type: 'bool', probability: p[q] }), () => yes);
  const decl: Declaration = { scopes: { rows: scope }, checks: { kept: { scope: 'rows', question: { type: 'bool', instructions: 'Does it keep the rule?' }, aggregate: { name: 'all' } } } };
  const v = await rejudge(api, opts(rubricOf('kept', -3), decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.equal(requests.length, 1, 'units sharing the guide are packed into one request');
  assert.deepEqual(requests[0].state, { context: { guide: { rule: 'r' } }, items: [{ id: 'rows a', row: { id: 'a' } }, { id: 'rows b', row: { id: 'b' } }, { id: 'rows c', row: { id: 'c' } }] });
  assert.deepEqual(escalated, [], 'the fold had decided, so nothing escalated');
  const k = v.checks[0];
  assert.deepEqual([k.met, k.notes, k.undecidedItems, k.resolvers, k.splitter], [true, ['rows a: flagged by clef (no reason given)'], ['rows b'], { clef: 2 }, 'jsonItems']);
  assert.equal(v.passed, false);
  assert.deepEqual(v.undecided, []);
});

test('while the fold can go either way the unsure item escalates; when nobody can answer, the check is undecided, not failed', async () => {
  const p: Record<string, number> = { 'c.kept#0': 0.95, 'c.kept#1': 0.5, 'c.kept#2': 0.95 };
  const decl: Declaration = { scopes: { rows: scope }, checks: { kept: { scope: 'rows', question: { type: 'bool', instructions: 'Does it keep the rule?' }, aggregate: { name: 'all' } } } };
  const answered = fakeApi((q) => ({ type: 'bool', probability: p[q] }), () => yes);
  const v = await rejudge(answered.api, opts(rubricOf('kept', 2), decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual(answered.escalated, ['Does it keep the rule?']);
  assert.deepEqual([v.checks[0].met, v.checks[0].resolvers, v.passed], [true, { clef: 2, 'judge-model': 1 }, true]);
  const silent = fakeApi((q) => ({ type: 'bool', probability: p[q] }), () => 'no idea');
  const u = await rejudge(silent.api, opts(rubricOf('kept', 2), decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual([u.checks[0].met, u.checks[0].undecided, u.undecided], [null, 1, ['c.kept']]);
});

test('a choice question holds on its pass options, from the decision model or the judge model', async () => {
  const probabilities: Record<string, Record<string, number>> = { 'c.grade#0': { good: 0.6, fine: 0.3, bad: 0.1 }, 'c.grade#1': { good: 0.1, fine: 0.1, bad: 0.8 }, 'c.grade#2': { good: 0.5, fine: 0, bad: 0.5 } };
  const { api, escalated } = fakeApi((q) => ({ type: 'choice', choice: 'good', confidence: 1, probabilities: probabilities[q] }), () => JSON.stringify({ choice: 'fine', confidence: 0.9, reason: 'ok' }));
  const question = { type: 'choice' as const, instructions: 'Grade it.', options: { good: 'it is good', fine: 'it will do', bad: 'it is not' } };
  const decl: Declaration = { scopes: { rows: scope }, checks: { grade: { scope: 'rows', question, aggregate: { name: 'atLeast', k: 2, pass: ['good', 'fine'] } } } };
  const v = await rejudge(api, opts(rubricOf('grade', 3), decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual(escalated, ['Grade it.'], 'c is a coin toss between holding and not, so it escalates');
  assert.deepEqual([v.checks[0].met, v.checks[0].notes], [2 / 3, ['rows b: flagged by clef (no reason given)']]);
});

test('coverageUnion by related counts what code found and asks no model; a declaration that misses a check is refused', async () => {
  const { api, requests } = fakeApi(() => ({ type: 'bool', probability: 0.5 }), () => yes);
  const decl: Declaration = {
    scopes: { rows: { split: { name: 'jsonItems', from: 'rows' }, context: { cite: { name: 'related', chunker: { name: 'listItems', from: 'review' }, match: 'overlap', min: 1 } } } },
    checks: { cited: { scope: 'rows', question: { type: 'bool', instructions: 'Is it cited?' }, aggregate: { name: 'coverageUnion', by: 'related', min: 0.5 } } },
  };
  const v = await rejudge(api, opts(rubricOf('cited', 2), decl, { rows: JSON.stringify([{ id: 'a', text: 'alpha change' }, { id: 'b', text: 'beta change' }]), review: '- alpha change looks right\n' }), sealed, 'm', BACKGROUND_CONTEXT);
  assert.equal(requests.length, 0);
  assert.deepEqual([v.checks[0].met, v.checks[0].resolvers], [true, { code: 2 }]);
  await assert.rejects(rejudge(api, opts(rubricOf('other', 2), decl), sealed, 'm', BACKGROUND_CONTEXT), /no declaration for other; declared checks the rubric lacks: cited/);
});

test('the llm splitter asks the judge model once per sealed version: a rejudge under the same memo reuses its split', async () => {
  const split = JSON.stringify([{ item: 'the bus count', span: 'The fleet has 38 buses.' }]);
  const { api, escalated } = fakeApi(() => ({ type: 'bool', probability: 0.95 }), (name) => (name === 'split' ? `Here: ${split}` : yes));
  const decl: Declaration = { scopes: { claims: { split: { name: 'llm', from: 'report', instruction: 'one factual claim' } } }, checks: { true_claim: { scope: 'claims', question: { type: 'bool', instructions: 'Is it true?' }, aggregate: { name: 'all' } } } };
  const o = opts(rubricOf('true_claim', 2), decl, { report });
  const first = await rejudge(api, o, sealed, 'm', BACKGROUND_CONTEXT);
  await rejudge(api, o, sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual(escalated, ['split'], 'one model split, then the memo');
  assert.deepEqual([first.checks[0].met, first.checks[0].units, first.checks[0].splitter], [true, 1, 'llm']);
});

test('a share pitfall asks every unsure item even after a flag, and costs its weight times the share flagged', async () => {
  // Clef: a is flagged outright, b is unsure, c holds; the judge model clears b.
  const p: Record<string, number> = { 'c.kept#0': 0.05, 'c.kept#1': 0.5, 'c.kept#2': 0.95 };
  const decl: Declaration = { scopes: { rows: scope }, checks: { kept: { scope: 'rows', question: { type: 'bool', instructions: 'Does it keep the rule?' }, aggregate: { name: 'share' } } } };
  const { api, escalated } = fakeApi((q) => ({ type: 'bool', probability: p[q] }), () => yes);
  const v = await rejudge(api, opts({ ...rubricOf('kept', -3), criteria: [{ id: 'c', checks: [{ id: 'kept', label: 'kept', weight: -3 }, { id: 'good', label: 'good', weight: 4, code: () => ({ met: 1 }) }] }] }, decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual(escalated, ['Does it keep the rule?'], 'b still escalates: the share is not known until it is answered');
  const k = v.checks.find((c) => c.id === 'kept')!;
  assert.deepEqual([k.met, k.notes, k.undecidedItems], [1 / 3, ['rows a: flagged by clef (no reason given)'], undefined]);
  assert.deepEqual([v.score, v.passed], [(4 - 3 / 3) / 4, false]);
  const silent = fakeApi((q) => ({ type: 'bool', probability: p[q] }), () => 'no idea');
  const u = await rejudge(silent.api, opts(rubricOf('kept', -3), decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual([u.checks[0].met, u.checks[0].undecided, u.undecided], [null, 1, ['c.kept']], 'an item nobody answers leaves the share undecided');
});
