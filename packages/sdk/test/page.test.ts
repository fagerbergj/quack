import assert from 'node:assert/strict';
import { test } from 'node:test';
import { isSessionKey, PageError, pageCursor, SESSION_KEY } from '../src/index.ts';

test('a token round-trips, no token starts the list, and the token is the base64url of [list, value]', () => {
  const c = pageCursor('things');
  assert.equal(c.decode(null), undefined);
  assert.equal(c.decode(''), undefined);
  const token = c.encode('a b/ü');
  assert.deepEqual(JSON.parse(Buffer.from(token, 'base64url').toString()), ['things', 'a b/ü']);
  assert.equal(c.decode(token), 'a b/ü');
});

test('a foreign list, a mangled token or one that does not re-encode to itself is a PageError', () => {
  const c = pageCursor('things');
  const mint = (v: unknown) => Buffer.from(JSON.stringify(v)).toString('base64url');
  const bad = [pageCursor('other').encode('x'), 'garbage', '!!', mint(['things', 3]), mint({ a: 1 }), mint('things'), `${c.encode('x')}=`, Buffer.from('["things","x"] ').toString('base64url'), Buffer.from('2026-01-01').toString('base64url')];
  for (const token of bad) assert.throws(() => c.decode(token), PageError, token);
});

test('pageSize defaults to 100 and accepts 1 to 1000 integers only', () => {
  const c = pageCursor('things');
  assert.equal(c.pageSize(null), 100);
  assert.deepEqual(['1', '1000', '007'].map((s) => c.pageSize(s)), [1, 1000, 7]);
  for (const bad of ['0', '1001', '-1', 'x', '1.5', '', ' 5', '1e2']) assert.throws(() => c.pageSize(bad), PageError, bad);
});

test('isSessionKey takes 1 to 200 characters of the grammar and nothing else', () => {
  assert.equal(isSessionKey(''), false);
  assert.equal(isSessionKey('a'), true);
  assert.equal(isSessionKey('a'.repeat(200)), true);
  assert.equal(isSessionKey('a'.repeat(201)), false);
  for (const ch of 'AZaz09:/#._@-') assert.equal(isSessionKey(`k${ch}`), true, ch);
  for (const ch of [' ', '\n', '?', '%', '\\', '+', 'é', '*']) assert.equal(isSessionKey(`k${ch}`), false, JSON.stringify(ch));
  assert.equal(SESSION_KEY.test('sleeper-waivers:w9'), true);
});
