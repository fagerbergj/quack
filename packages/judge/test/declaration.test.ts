import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { buildEvidence, checkDeclaration, PASS_ONLY, type Declaration } from '../src/declaration.ts';
import { fold, metOf } from '../src/fold.ts';
import { registerSelector, registerSplitter, rejudge, type Inputs, type JudgeOptions, type Rubric } from '../src/index.ts';

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
});

test('met: a positive check holds when the fold is true, a pitfall is present when it is false; share reports the share', () => {
  assert.equal(metOf({ name: 'all' }, true, [T, T], false), true);
  assert.equal(metOf({ name: 'all' }, false, [T, F], true), true, 'a failing item makes the pitfall present');
  assert.equal(metOf({ name: 'any' }, true, [T, F], true), false);
  assert.equal(metOf({ name: 'share', min: 0.5 }, true, [T, T, F, U], false), 0.5);
  assert.equal(metOf({ name: 'share', min: 0.5 }, true, [T, T, F, U], true), 0.25, "a pitfall's share is of the items where the claim failed");
});

const report = '# Title\nIntro.\n## Findings\n- Buses cost $2 million each. [s1]\n- The fleet has 38 buses.\n- ok\n## Notes\n- skip me please now\n';
const cards = JSON.stringify({ rows: [{ id: 'r1', player: 'p1', action: 'start' }, { id: 'r2', player: 'p2', action: 'keep' }, { id: 'r3', player: 'p3', action: 'sit' }] });
const facts = JSON.stringify({ p1: { injured: true }, p2: { injured: false }, p3: { injured: false } });
const inputs: Inputs = { report, cards, facts, intent: JSON.stringify({ purpose: 'judge', guarantees: ['g'] }) };

test('a scope builds one unit per item: the piece, item context in state, context every item sees identically in shared', () => {
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
  const { evidence, splits } = buildEvidence(decl, inputs);
  assert.deepEqual(evidence.rows.map((u) => u.id), ['rows r1', 'rows r2'], 'ids are scope-qualified and the where dropped the sit row');
  assert.deepEqual(evidence.rows[0], { id: 'rows r1', state: { row: { id: 'r1', player: 'p1', action: 'start' }, facts: { injured: true } }, related: { facts: 1 }, shared: { intent: 'judge' } });
  assert.deepEqual(evidence.whole[0].id, 'whole intent');
  assert.deepEqual(evidence.whole[0].state, { intent: { purpose: 'judge', guarantees: ['g'] } }, 'JSON text goes to the model as JSON');
  assert.deepEqual(splits.rows, { splitter: 'jsonItems' });
});

test('a cross-input where compares an item field against its joined record', () => {
  const decl: Declaration = {
    scopes: {
      injured: { split: { name: 'jsonItems', from: 'cards', path: 'rows', where: [{ path: 'action', op: 'ne', as: 'facts', field: 'injured' }] }, context: { facts: { name: 'join', from: 'facts', by: 'player' } } },
    },
    checks: {},
  };
  const { evidence } = buildEvidence(decl, inputs);
  assert.deepEqual(evidence.injured.map((u) => u.id), ['injured r1', 'injured r2', 'injured r3'], 'an action is never equal to an injury flag');
});

test('text splitters read sections, strip, match and cap, and a declared cut is marked in the context', () => {
  const decl: Declaration = {
    scopes: {
      claims: { split: { name: 'sentences', from: 'report', skip: ['Notes'], match: '\\d', max: 1 }, context: { notes: { name: 'sections', from: 'report', names: ['Notes'], maxChars: 6 } } },
      sections: { split: { name: 'markdownSections', from: 'report', names: ['notes'] } },
    },
    checks: {},
  };
  const { evidence } = buildEvidence(decl, inputs);
  assert.deepEqual(evidence.claims.map((u) => u.state.piece), ['Buses cost $2 million each. [s1]']);
  assert.equal(evidence.claims[0].shared!.notes, '- skip\n… (cut at 6 characters)');
  assert.deepEqual(evidence.sections.map((u) => u.id), ['sections Notes']);
});

test('map-reduce pairs passages whose digests conflict', () => {
  const text = '- The fleet has 38 electric buses in service.\n- The fleet has 42 electric buses in service.\n- Tickets cost $2.\n';
  const decl: Declaration = { scopes: { pairs: { split: { name: 'digestPairs', from: 'x', of: { name: 'listItems', from: 'x' }, extract: 'numbers', compare: 'sameTermsOtherValue' } } }, checks: {} };
  const { evidence } = buildEvidence(decl, { x: text });
  assert.deepEqual(evidence.pairs.map((u) => [u.id, u.state.piece]), [['pairs item-1|item-2', { first: 'The fleet has 38 electric buses in service.', second: 'The fleet has 42 electric buses in service.' }]]);
});

test('map-reduce compares a quantity by its figure, whatever its unit word', () => {
  const text = '- Each bus is rated at 190 km per charge.\n- Each bus is rated at 190 per charge.\n- Eligible farms are under 40 acres.\n- Eligible farms are under 60 acres.\n';
  const decl: Declaration = { scopes: { pairs: { split: { name: 'digestPairs', from: 'x', of: { name: 'listItems', from: 'x' }, extract: 'numbers', compare: 'sameTermsOtherValue' } } }, checks: {} };
  const { evidence } = buildEvidence(decl, { x: text });
  assert.deepEqual(evidence.pairs.map((u) => u.id), ['pairs item-3|item-4']);
});

test('a declaration error is loud: an input nobody provided, a splitter nobody registered, a second strategy under one name', () => {
  assert.throws(() => buildEvidence({ scopes: { s: { split: { name: 'whole', from: 'nope' } } }, checks: {} }, inputs), /reads input "nope"/);
  assert.throws(() => buildEvidence({ scopes: { s: { split: { name: 'nope', from: 'report' } } }, checks: {} }, inputs), /no splitter "nope"/);
  const mine = () => [];
  registerSplitter('test.mine', mine);
  registerSplitter('test.mine', mine);
  assert.throws(() => registerSplitter('test.mine', () => [{ id: 'x', text: 'x' }]), /already registered/);
  assert.throws(() => registerSplitter('whole', () => []), /library's own/);
  // A replay arm loads the extension again from another tree: a new function object with the same source.
  registerSplitter('test.mine', () => []);
});

test('a bool pitfall asked as the defect holds on "no": a confident yes flags the item, from either model', async () => {
  const p: Record<string, number> = { q0: 0.9, q1: 0.1, q2: 0.5 };
  const { api, escalated } = fakeApi((q) => ({ type: 'bool', probability: p[q] }), () => yes);
  const decl: Declaration = { scopes: { rows: scope }, checks: { broken: { scope: 'rows', question: { type: 'bool', instructions: 'Is it broken?', pass: ['no'] }, aggregate: { name: 'share' } } } };
  const v = await rejudge(api, opts(rubricOf('broken', -3), decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual(escalated, ['Is it broken?']);
  assert.deepEqual([v.checks[0].met, v.checks[0].notes], [2 / 3, ['rows a: flagged by clef (no reason given)', 'rows c: fine']]);
});

// A tool call's api as the judge reads it: memos, a decision model answering by question number and a judge model, with every request recorded.
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
  kind: 'k', rubric, declaration, inputs: async () => ({ rows, guide: '{"rule":"r"}', ...more }), maxRevisions: 0,
  decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 }, judgeModel: { provider: 'judge', modelId: 'j' },
});
const yes = JSON.stringify({ choice: 'yes', confidence: 0.9, reason: 'fine' });
const rubricOf = (id: string, weight: number | 'gate'): Rubric => ({ name: 'r', version: 1, threshold: 0.8, criteria: [{ id: 'c', checks: [{ id, label: id, weight }] }] });
const scope = { split: { name: 'jsonItems', from: 'rows' }, piece: 'row', context: { guide: { name: 'json', from: 'guide' } } };

test('a pitfall decided by one failing item skips the escalation the other unsure item would have needed, and lists it', async () => {
  // Clef: a fails the claim outright, b is unsure, c holds.
  const p: Record<string, number> = { q0: 0.05, q1: 0.5, q2: 0.95 };
  const { api, requests, escalated } = fakeApi((q) => ({ type: 'bool', probability: p[q] }), () => yes);
  const decl: Declaration = { scopes: { rows: scope }, checks: { kept: { scope: 'rows', question: { type: 'bool', instructions: 'Does it keep the rule?' }, aggregate: { name: 'all' } } } };
  const v = await rejudge(api, opts(rubricOf('kept', -3), decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.equal(requests.length, 1, 'units sharing the guide are packed into one request');
  assert.deepEqual(requests[0].state, { context: { guide: { rule: 'r' } }, asks: ['Does it keep the rule?'], items: [{ id: 'rows a', row: { id: 'a' } }, { id: 'rows b', row: { id: 'b' } }, { id: 'rows c', row: { id: 'c' } }] });
  assert.deepEqual(escalated, [], 'the fold had decided, so nothing escalated');
  const k = v.checks[0];
  assert.deepEqual([k.met, k.notes, k.undecidedItems, k.resolvers, k.splitter], [true, ['rows a: flagged by clef (no reason given)'], ['rows b'], { clef: 2 }, 'jsonItems']);
  assert.equal(v.passed, false);
  assert.deepEqual(v.undecided, []);
});

test('while the fold can go either way the unsure item escalates; when nobody can answer, the check is undecided, not failed', async () => {
  const p: Record<string, number> = { q0: 0.95, q1: 0.5, q2: 0.95 };
  const decl: Declaration = { scopes: { rows: scope }, checks: { kept: { scope: 'rows', question: { type: 'bool', instructions: 'Does it keep the rule?' }, aggregate: { name: 'all' } } } };
  const answered = fakeApi((q) => ({ type: 'bool', probability: p[q] }), () => yes);
  const v = await rejudge(answered.api, opts(rubricOf('kept', 2), decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual(answered.escalated, ['Does it keep the rule?']);
  assert.deepEqual([v.checks[0].met, v.checks[0].resolvers, v.passed], [true, { clef: 2, j: 1 }, true]);
  const silent = fakeApi((q) => ({ type: 'bool', probability: p[q] }), () => 'no idea');
  const u = await rejudge(silent.api, opts(rubricOf('kept', 2), decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual([u.checks[0].met, u.checks[0].undecided, u.undecided], [null, 1, ['c.kept']]);
});

test('a choice question holds on its pass options, from the decision model or the judge model', async () => {
  const probabilities: Record<string, Record<string, number>> = { q0: { good: 0.6, fine: 0.3, bad: 0.1 }, q1: { good: 0.1, fine: 0.1, bad: 0.8 }, q2: { good: 0.5, fine: 0, bad: 0.5 } };
  const { api, escalated } = fakeApi((q) => ({ type: 'choice', choice: 'good', confidence: 1, probabilities: probabilities[q] }), () => JSON.stringify({ choice: 'fine', confidence: 0.9, reason: 'ok' }));
  const question = { type: 'choice' as const, instructions: 'Grade it.', options: { good: 'it is good', fine: 'it will do', bad: 'it is not' } };
  const decl: Declaration = { scopes: { rows: scope }, checks: { grade: { scope: 'rows', question, aggregate: { name: 'share', pass: ['good', 'fine'] } } } };
  const v = await rejudge(api, opts(rubricOf('grade', 3), decl), sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual(escalated, ['Grade it.'], 'c is a coin toss between holding and not, so it escalates');
  assert.deepEqual([v.checks[0].met, v.checks[0].notes], [2 / 3, ['rows b: flagged by clef (no reason given)']]);
});

test('a declaration that misses a check or misconfigures decisionModel is refused', async () => {
  const { api } = fakeApi(() => ({ type: 'bool', probability: 0.5 }), () => yes);
  const decl: Declaration = {
    scopes: { rows: scope },
    checks: { cited: { scope: 'rows', question: { type: 'bool', instructions: 'Is it cited?' }, aggregate: { name: 'share' } } },
  };
  await assert.rejects(rejudge(api, opts(rubricOf('other', 2), decl), sealed, 'm', BACKGROUND_CONTEXT), /no declaration for other; declared checks the rubric lacks: cited/);
  const routed = { ...decl, checks: { cited: { ...decl.checks.cited, decisionModel: true as never } } };
  await assert.rejects(rejudge(api, opts(rubricOf('cited', 2), routed), sealed, 'm', BACKGROUND_CONTEXT), /decisionModel may only be false, pass-only or confident-only: cited/);
  assert.doesNotThrow(() => checkDeclaration({ ...decl, checks: { cited: { ...decl.checks.cited, decisionModel: PASS_ONLY } } }, ['cited']));
});

registerSelector('test.backed', (_i, chunk) => ({ value: '', matches: chunk.text.includes('backed') ? 1 : 0 }));
registerSelector('test.plain', (_i, chunk) => `seen ${chunk.id}`);
const sourced = (...texts: string[]) => ({ rows: JSON.stringify(texts.map((text, n) => ({ id: String.fromCharCode(97 + n), text }))) });
const backedDecl = (by: 'related' | 'either'): Declaration => ({
  scopes: { rows: { split: { name: 'jsonItems', from: 'rows', pick: ['text'] }, context: { src: { name: 'test.backed' } } } },
  checks: { named: { scope: 'rows', question: { type: 'bool', instructions: 'Is it named?' }, aggregate: { name: 'share', by } } },
});

test('a share by either: an item a selector matched holds without a model, the rest are asked, and the share is over all items', async () => {
  const { api, requests } = fakeApi(() => ({ type: 'bool', probability: 0.05 }), () => yes);
  const v = await rejudge(api, opts(rubricOf('named', 2), backedDecl('either'), sourced('backed', 'plain', 'backed too')), sealed, 'm', BACKGROUND_CONTEXT);
  assert.deepEqual(requests.map((r) => r.state), [{ piece: { text: 'plain' } }], 'only the unmatched item is asked, and the empty selector value adds no field');
  assert.deepEqual([v.checks[0].met, v.checks[0].resolvers], [2 / 3, { code: 2, clef: 1 }]);
});

test('a share by related asks no model, and is met, not null, when code settles every item', async () => {
  const { api, requests } = fakeApi(() => ({ type: 'bool', probability: 0.95 }), () => yes);
  const related = await rejudge(api, opts(rubricOf('named', 2), backedDecl('related'), sourced('backed', 'plain')), sealed, 'm1', BACKGROUND_CONTEXT);
  assert.deepEqual([related.checks[0].met, related.checks[0].resolvers], [0.5, { code: 2 }]);
  const settled = await rejudge(api, opts(rubricOf('named', 2), backedDecl('either'), sourced('backed', 'backed too')), sealed, 'm2', BACKGROUND_CONTEXT);
  assert.deepEqual([settled.checks[0].met, settled.checks[0].units], [1, 2]);
  assert.equal(requests.length, 0);
});

test('a registered selector that returns a string still adds its context, and counts no match', () => {
  const decl: Declaration = { scopes: { rows: { split: { name: 'jsonItems', from: 'rows' }, context: { seen: { name: 'test.plain' } } } }, checks: {} };
  const { evidence } = buildEvidence(decl, { rows: JSON.stringify([{ id: 'a' }]) });
  assert.deepEqual(evidence.rows[0], { id: 'rows a', state: { piece: { id: 'a' }, seen: 'seen a' } });
  const empty = buildEvidence(backedDecl('either'), sourced('backed'));
  assert.deepEqual(empty.evidence.rows[0], { id: 'rows a', state: { piece: { text: 'backed' } }, related: { src: 1 } }, 'an empty value adds no field but its matches count');
});

test('a share pitfall asks every unsure item even after a flag, and costs its weight times the share flagged', async () => {
  // Clef: a is flagged outright, b is unsure, c holds; the judge model clears b.
  const p: Record<string, number> = { q0: 0.05, q1: 0.5, q2: 0.95 };
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
