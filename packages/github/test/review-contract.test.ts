import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { test } from 'node:test';
import { Ajv2020 } from 'ajv/dist/2020.js';
import { parseReview, validateReview } from '../src/review.ts';

// Shared with the web's parser copy (packages/web/src/extensions/review.test.ts): both must read these reviews the same way.
const SHARED = new URL('../../../test-fixtures/review/', import.meta.url);
const CONTRACT = { key: 'contracts:038d6f48-11fe-48e3-8953-5a90e61b3f2c', version: 6 };
const schema = JSON.parse(readFileSync(new URL('./fixtures/review-contract-v6.json', import.meta.url), 'utf8'));
const accepts = new Ajv2020({ strict: false, validateFormats: false }).compile(schema);
const names = readdirSync(SHARED).filter((n) => n.endsWith('.md')).map((n) => n.slice(0, -3));
const md = (name: string) => readFileSync(new URL(`${name}.md`, SHARED), 'utf8');
const expected = (name: string) => JSON.parse(readFileSync(new URL(`${name}.json`, SHARED), 'utf8'));
const read = (name: string) => JSON.parse(JSON.stringify(parseReview(md(name))));

test(`the checked-in schema is ${CONTRACT.key} v${CONTRACT.version}`, () => {
  assert.equal(schema.title, 'github.review — the Markdown review in the form parseReview returns');
  assert.ok(names.length >= 20);
});

test('every shared fixture parses to its recorded reading and validates as recorded', () => {
  for (const n of names) {
    assert.deepEqual(read(n), expected(n).parse, n);
    assert.deepEqual(validateReview(md(n)), expected(n).problems, n);
  }
});

test('the contract schema accepts a parse exactly when the validator seals the review', () => {
  for (const n of names) assert.equal(accepts(read(n)), validateReview(md(n)).length === 0, `${n}: ${JSON.stringify(accepts.errors)}`);
});

test('a fenced block is its finding\'s continuation, whatever its lines start with', () => {
  for (const n of ['fence-diff', 'fence-tilde', 'fence-unclosed', 'fence-before-finding']) assert.deepEqual(parseReview(md(n)).unparsed, [], n);
  const [first] = parseReview(md('fence-diff')).findings;
  assert.match(first.text, /\n- if \(ok\) return;\n\+ if \(!ok\) return;\n- nit: this is code, not a finding\n```$/);
  assert.match(parseReview(md('fence-blank-lines')).findings[0].text, /const a = 1;\n\nconst b = 2;\n\n\nrun\(a, b\);\n```$/);
  assert.match(parseReview(md('fence-blank-after-open')).findings[0].text, /\n~~~\n\n- old\n/);
  assert.equal(parseReview(md('fence-unclosed')).findings.length, 1);
  assert.deepEqual(parseReview(md('fence-before-finding')).findings.map((f) => f.text), ['the first finding.']);
});

test('a bullet-shaped line outside a fence fails the seal naming the line; other pre-finding prose is dropped', () => {
  for (const [n, line] of [['bullet-star', '* a star item'], ['bullet-plus', '+ a plus item'], ['bullet-number', '1. a numbered item'], ['bullet-before-finding', '* a star item before any finding']]) {
    assert.deepEqual(parseReview(md(n)).unparsed, [line], n);
    assert.match(validateReview(md(n)).join('\n'), new RegExp(`not a finding line .*: ${line.replace(/[*+.]/g, '\\$&')}`), n);
  }
  assert.deepEqual(validateReview(md('prose-before-finding')), []);
  assert.deepEqual(validateReview(md('real-none-in-parens')), []);
});
