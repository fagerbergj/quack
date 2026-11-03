import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import type { GithubApp } from '../src/app.ts';
import { pullStates } from '../src/pulls.ts';

const DAY = 24 * 3600_000;
const fake = (reads: string[]) => ({ asInstallation: async (_o: string, _r: string, _m: string, path: string) => (reads.push(path), { json: { state: 'open', updated_at: 'u' } }) }) as unknown as GithubApp;

test('an open state older than the TTL is read again; a fresh one is not', async () => {
  let t = 0;
  const reads: string[] = [];
  const s = pullStates(undefined, () => t);
  await s.lookup(fake(reads), ['a/b#1']);
  t = 9 * 60_000;
  await s.lookup(fake(reads), ['a/b#1']);
  assert.equal(reads.length, 1);
  t = 11 * 60_000;
  await s.lookup(fake(reads), ['a/b#1']);
  assert.equal(reads.length, 2);
});

test('merged and closed entries older than 30 days are dropped on save, open ones stay', () => {
  const file = join(mkdtempSync(join(tmpdir(), 'pulls-')), 'p.json');
  let t = 0;
  const s = pullStates(file, () => t);
  s.record('a/b#1', 'merged', 'x');
  s.record('a/b#2', 'open', 'x');
  t = 31 * DAY;
  s.record('a/b#3', 'closed', 'x');
  assert.deepEqual(Object.keys(JSON.parse(readFileSync(file, 'utf8'))).sort(), ['a/b#2', 'a/b#3']);
});

test('more than 100 keys are cut to 100 and the reply says so', async () => {
  const reads: string[] = [];
  const keys = Array.from({ length: 101 }, (_, i) => `a/b#${i + 1}`);
  const out = await pullStates().lookup(fake(reads), keys);
  assert.equal(reads.length, 100);
  assert.equal(out.truncated, true);
  assert.equal((await pullStates().lookup(fake(reads), keys.slice(0, 100))).truncated, undefined);
});
