import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { cardProblems } from '../src/cards.ts';
import { cardsKind } from '../src/lineup/card.ts';

const CONTRACT = { key: 'contracts:5f1e60fc-d8f3-463e-9460-4cc02763a40c', version: 5 };
const read = (name: string) => readFileSync(new URL(`./fixtures/${name}`, import.meta.url), 'utf8');
const schema = JSON.parse(read('cards-contract-v5.json'));
const accepts = new Ajv2020({ strict: false, validateFormats: false }).compile(schema);
const cards: Array<Record<string, any>> = read('cards.ndjson').trim().split('\n').map((l) => JSON.parse(l));
const KINDS = ['lineup', 'waivers', 'digest', 'retro', 'trade-finder', 'trends', 'history'];
const first = (kind: string) => cards.find((c) => c.kind === kind)!;

test(`the checked-in schema is ${CONTRACT.key} v${CONTRACT.version}, and the captured cards cover all seven kinds, both waiver sizes and a season with its cited draft`, () => {
  assert.equal(schema.title, 'Sleeper card');
  assert.deepEqual([...new Set(cards.map((c) => c.kind))].sort(), [...KINDS].sort());
  assert.deepEqual([...new Set(cards.flatMap((c) => c.candidates ?? []).map((x: any) => x.size.kind))].sort(), ['faab', 'priority']);
  assert.ok(cards.some((c) => c.seasons?.some((s: any) => s.cited)));
});

test('every card a builder produced passes both the seal check and the contract schema', () => {
  for (const c of cards) {
    assert.deepEqual(cardProblems(c), [], c.kind);
    assert.equal(accepts(c), true, `${c.kind}: ${JSON.stringify(accepts.errors)}`);
    assert.deepEqual(cardsKind.validate!(JSON.stringify(c)), []);
  }
});

type Edit = { kind: string; path: Array<string | number>; set?: unknown; says: RegExp };
const GONE = Symbol('delete');
const REFUSED: Edit[] = [
  { kind: 'lineup', path: ['summary'], set: GONE, says: /card\.summary is required \(expected a string\)/ },
  { kind: 'lineup', path: ['starters', 0, 'proj'], set: '9', says: /card\.starters\[0\]\.proj: expected a number, got string "9"/ },
  { kind: 'lineup', path: ['starters', 0, 'action'], set: 'bench', says: /card\.starters\[0\]\.action: expected one of start, keep/ },
  { kind: 'lineup', path: ['starters', 0, 'player', 'team'], set: GONE, says: /starters\[0\]\.player\.team is required \(expected a string or null\)/ },
  { kind: 'waivers', path: ['teams'], set: GONE, says: /card\.teams is required \(expected an integer\)/ },
  { kind: 'waivers', path: ['candidates', 0, 'rank'], set: '1', says: /candidates\[0\]\.rank: expected an integer/ },
  { kind: 'waivers', path: ['waiver_type'], set: 'open', says: /card\.waiver_type: expected one of rolling, reverse_standings, faab/ },
  { kind: 'waivers', path: ['candidates', 0, 'drop', 'role'], set: 'taxi', says: /drop\.role: expected one of starter, bench, ir/ },
  { kind: 'waivers', path: ['candidates', 0, 'size', 'kind'], set: 'bid', says: /size\.kind: expected one of faab, priority/ },
  { kind: 'digest', path: ['playoff_line'], set: GONE, says: /card\.playoff_line is required \(expected an object\)/ },
  { kind: 'digest', path: ['games', 0, 'margin'], set: 'close', says: /games\[0\]\.margin: expected a number/ },
  { kind: 'digest', path: ['past'], set: 'no', says: /card\.past: expected a boolean/ },
  { kind: 'retro', path: ['lessons'], set: GONE, says: /card\.lessons is required \(expected an array\)/ },
  { kind: 'retro', path: ['won'], set: 'yes', says: /card\.won: expected a boolean/ },
  { kind: 'retro', path: ['lessons'], set: ['a', 'b', 'c', 'd'], says: /card\.lessons: expected at most 3 items, got 4/ },
  { kind: 'trade-finder', path: ['team'], set: GONE, says: /card\.team is required \(expected a string\)/ },
  { kind: 'trade-finder', path: ['week'], set: '4', says: /card\.week: expected an integer/ },
  { kind: 'trade-finder', path: ['suggestions', 0, 'partner', 'stance'], set: 'neutral', says: /partner\.stance: expected one of contender, rebuilder/ },
  { kind: 'trends', path: ['since'], set: GONE, says: /card\.since is required \(expected a string or null\)/ },
  { kind: 'trends', path: ['week'], set: '11', says: /card\.week: expected an integer/ },
  { kind: 'trends', path: ['items', 0, 'kind'], set: 'rumor', says: /items\[0\]\.kind: expected one of injury, waiver, news, note/ },
  { kind: 'history', path: ['definitions'], set: GONE, says: /card\.definitions is required \(expected an object\)/ },
  { kind: 'history', path: ['week'], set: '3', says: /card\.week: expected an integer/ },
  { kind: 'history', path: ['seasons', 0, 'weeks', 0, 'result'], set: 'X', says: /weeks\[0\]\.result: expected one of W, L, T, got string "X"/ },
  { kind: 'history', path: ['seasons', 0, 'cited'], set: GONE, says: /seasons\[0\]\.cited is required \(expected an array\)/ },
];

test('a card with a missing field, a wrong type or a value outside an enum is refused by the seal and by the schema, naming the field', () => {
  for (const e of REFUSED) {
    const card = structuredClone(first(e.kind));
    const parent = e.path.slice(0, -1).reduce((o, k) => o[k], card as any);
    const last = e.path[e.path.length - 1];
    if (e.set === GONE) delete parent[last];
    else parent[last] = e.set;
    const problems = cardProblems(card);
    assert.ok(problems.some((p) => e.says.test(p)), `${e.kind} ${e.path.join('.')}: ${problems.join(' | ')}`);
    assert.equal(accepts(card), false, `${e.kind} ${e.path.join('.')} passed the schema`);
    assert.deepEqual(cardsKind.validate!(JSON.stringify(card)), problems);
  }
});

test('the seal refuses a trade card, an unknown kind and text that is not JSON', () => {
  const trade = { kind: 'trade', league: 'L', summary: 's', offers: [] };
  assert.match(cardProblems(trade)[0], /card\.kind: expected one of lineup, waivers, digest, retro, trade-finder, trends, history, got string "trade"/);
  assert.equal(accepts(trade), false);
  assert.match(cardProblems([])[0], /card: expected an object, got an array/);
  assert.deepEqual(cardsKind.validate!('nope'), ['cards.json is not JSON']);
});

test('objects stay open: a field the contract does not name is accepted', () => {
  const card = { ...structuredClone(first('lineup')), extra: 1 };
  assert.deepEqual(cardProblems(card), []);
  assert.equal(accepts(card), true);
});
