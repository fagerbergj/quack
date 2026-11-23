import assert from 'node:assert/strict';
import { test } from 'node:test';
import { factProblems, type Change, type Fact } from '../src/index.ts';

type Lists = { guarantees?: Fact[]; nonGoals?: Fact[] };
const spec = ({ guarantees = [], nonGoals = [] }: Lists, facts?: string[][]) => ({
  intent: { guarantees, nonGoals },
  changes: (facts ?? []).map((f): Change => ({ choice: 'c', was: 'w', now: 'n', why: 'y', facts: f })),
});
// The template contract.spec v13 records in factEntry's $comment, written out apart from the code's.
const REASON = [
  'over 160 — the text is \\d+ characters, so split it into one fact per entry', 'no id — write it as an object with an "id" key and a "text" key, e\\.g\\. \\{"id": "gate\\.names-only", "text": "\\.\\.\\."\\} — a Label: text string has no id',
  'no id — add an "id" key, lowercase dotted or dashed, e\\.g\\. "gate\\.names-only"',
  "bad id — '[^']*' is not lowercase dotted or dashed, so rename it", "duplicate id — '[^']*' is used more than once, so give each entry its own id",
  "renamed id — the text matches the baseline's id '[^']*', so keep that id",
  "id named in no changes row's facts — '[^']*' was added with new text, changed or removed, so name it in the row that touches the list",
  "facts id the revision did not touch — '[^']*' was named but was neither added, changed nor removed, and is not an id given to unchanged text, so remove it from facts",
];
const TEMPLATE = new RegExp(`^facts are not decomposed: (?:(?:guarantees|nonGoals)\\[\\d+\\]|\\(id '[^']*'\\)|changes\\[\\d+\\]\\.facts): (?:${REASON.join('|')})$`);
const STR_ID = 'no id — write it as an object with an "id" key and a "text" key, e.g. {"id": "gate.names-only", "text": "..."} — a Label: text string has no id';
const OBJ_ID = 'no id — add an "id" key, lowercase dotted or dashed, e.g. "gate.names-only"';
const gate = (now: ReturnType<typeof spec>, base?: Lists) => {
  const out = factProblems(now, base && { intent: base });
  for (const m of out) assert.match(m, TEMPLATE);
  return out;
};
const msg = (at: string, reason: string) => `facts are not decomposed: ${at}: ${reason}`;
const unnamed = (at: string, id: string) => msg(at, `id named in no changes row's facts — '${id}' was added with new text, changed or removed, so name it in the row that touches the list`);

// Shapes the 31 approved specs on prod have: strings of every length (one of 2,762), duplicate texts, both lists.
const LONG = `Item reuse: ${'an item already decided is not asked again; '.repeat(60)}`;
const stored: Lists = { guarantees: ['Read-only', LONG, 'x'.repeat(185), 'Read-only'], nonGoals: ['Editing schedules', 'y'.repeat(338)] };
const T = 'A finding carries a path and line or neither.';
const U = 'version.lock is machine-written.';

test('an unchanged revision passes, as do its lists reordered', () => {
  const ided: Lists = { guarantees: [{ id: 'reuse.key', text: T }, 'Read-only'], nonGoals: [{ id: 'no-ui', text: U }] };
  for (const base of [stored, ided]) {
    assert.deepEqual(gate(spec(base), base), []);
    assert.deepEqual(gate(spec({ guarantees: base.guarantees!.toReversed(), nonGoals: base.nonGoals!.toReversed() }), base), []);
  }
});

test('a one-character edit to a stored string fails until it is an id\'d fact within the limit, named in a changes row', () => {
  assert.deepEqual(gate(spec({ guarantees: ['Read-only!'] }), { guarantees: ['Read-only'] }), [msg('guarantees[0]', STR_ID)]);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'read-only', text: `${'x'.repeat(185)}!` }] }, [['read-only']]), { guarantees: ['x'.repeat(185)] }), [msg('guarantees[0]', 'over 160 — the text is 186 characters, so split it into one fact per entry')]);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'read-only', text: 'Read-only!' }] }), { guarantees: ['Read-only'] }), [unnamed('guarantees[0]', 'read-only')]);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'read-only', text: 'Read-only!' }] }, [['read-only']]), { guarantees: ['Read-only'] }), []);
});

test('splitting a long string into facts fails without facts and passes with them; removing a stored string is not checked', () => {
  const parts = ['reuse.what', 'reuse.key', 'reuse.scope'].map((id) => ({ id, text: `The ${id} rule.` }));
  const split = { guarantees: ['Read-only', ...parts], nonGoals: stored.nonGoals };
  assert.deepEqual(gate(spec(split), stored), parts.map((p, i) => unnamed(`guarantees[${i + 1}]`, p.id)));
  assert.deepEqual(gate(spec(split, [['reuse.what', 'reuse.key'], ['reuse.scope']]), stored), []);
});

test('an unchanged entry keeps its id: a new id or a bare string is a rename', () => {
  const base = { guarantees: [{ id: 'reuse.key', text: T }] };
  const renamed = msg('guarantees[0]', "renamed id — the text matches the baseline's id 'reuse.key', so keep that id");
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'reuse.keys', text: T }] }, [['reuse.keys']]), base), [renamed, msg('changes[0].facts', "facts id the revision did not touch — 'reuse.keys' was named but was neither added, changed nor removed, and is not an id given to unchanged text, so remove it from facts")]);
  assert.deepEqual(gate(spec({ guarantees: [T] }), base), [renamed]);
});

test('an id given to unchanged text passes named or unnamed; naming an untouched id fails', () => {
  const now = { guarantees: [{ id: 'read-only', text: 'Read-only' }, 'Editing'] };
  assert.deepEqual(gate(spec(now), { guarantees: ['Read-only', 'Editing'] }), []);
  assert.deepEqual(gate(spec(now, [['read-only']]), { guarantees: ['Read-only', 'Editing'] }), []);
  assert.deepEqual(gate(spec(now, [[], ['read-only']]), now), [msg('changes[1].facts', "facts id the revision did not touch — 'read-only' was named but was neither added, changed nor removed, and is not an id given to unchanged text, so remove it from facts")]);
});

test('a changed text, a move between the lists and a removed id must each be named', () => {
  const base = { guarantees: [{ id: 'a', text: T }, { id: 'b', text: U }] };
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'a', text: `${T} Always.` }, { id: 'b', text: U }] }), base), [unnamed('guarantees[0]', 'a')]);
  const moved = { guarantees: [{ id: 'a', text: T }], nonGoals: [{ id: 'b', text: U }] };
  assert.deepEqual(gate(spec(moved), base), [unnamed('nonGoals[0]', 'b')]);
  assert.deepEqual(gate(spec(moved, [['b']]), base), []);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'a', text: T }] }), base), [unnamed("(id 'b')", 'b')]);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'a', text: T }] }, [['b']]), base), []);
});

test('duplicate baseline texts match each baseline entry once, by id first', () => {
  const base = { guarantees: [{ id: 'a', text: T }, { id: 'b', text: T }] };
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'b', text: T }, { id: 'a', text: T }] }), base), []);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'a', text: T }, { id: 'c', text: T }] }), base), [msg('guarantees[1]', "renamed id — the text matches the baseline's id 'b', so keep that id")]);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'a', text: T }] }), base), [unnamed("(id 'b')", 'b')]);
  assert.deepEqual(gate(spec({ guarantees: [T, T, T] }), { guarantees: [T, T] }), [msg('guarantees[2]', STR_ID)]);
});

test('with no approved spec every entry is an id\'d fact, ids unique across both lists, counted in code points', () => {
  assert.deepEqual(gate(spec({ guarantees: ['Read-only'], nonGoals: [{ id: 'a', text: T }] })), [msg('guarantees[0]', STR_ID)]);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'a', text: T }], nonGoals: [{ id: 'a', text: U }] })), [msg('nonGoals[0]', "duplicate id — 'a' is used more than once, so give each entry its own id")]);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'Reuse.Key', text: T }, { text: U } as never] })), [msg('guarantees[0]', "bad id — 'Reuse.Key' is not lowercase dotted or dashed, so rename it"), msg('guarantees[1]', OBJ_ID)]);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'emoji', text: '😀'.repeat(160) }] })), []);
  assert.deepEqual(gate(spec({ guarantees: [{ id: 'emoji', text: '😀'.repeat(161) }] })), [msg('guarantees[0]', 'over 160 — the text is 161 characters, so split it into one fact per entry')]);
});

test('a Label: text string gets the string reason and an {text} object the object reason', () => {
  assert.deepEqual(gate(spec({ guarantees: ['Label: text'] })), [msg('guarantees[0]', STR_ID)]);
  assert.deepEqual(gate(spec({ guarantees: [{ text: 'x' } as never] })), [msg('guarantees[0]', OBJ_ID)]);
});
