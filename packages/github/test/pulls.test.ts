import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import type { GithubApp } from '../src/app.ts';
import { pullStates, type PullEvent } from '../src/pulls.ts';

const DAY = 24 * 3600_000;
const open = { number: 1, state: 'open', merged: false, merged_at: null, closed_at: null, html_url: 'https://github.com/A/B/pull/1', title: 't', base: { repo: { full_name: 'A/B' } } };
// A fake GitHub whose answer a test changes between reads.
function fake(reads: string[], answer: Record<string, unknown> = { ...open }) {
  const app = { asInstallation: async (_o: string, _r: string, _m: string, path: string) => (reads.push(path), { json: { ...answer } }) } as unknown as GithubApp;
  return Object.assign(app, { set: (over: object) => Object.assign(answer, over) });
}
const watch = (s: ReturnType<typeof pullStates>) => {
  const events: PullEvent[] = [];
  s.subscribe((e) => events.push(e));
  return events;
};

test('an open PR older than the TTL is read again, a fresh one is not; owner and repo match in any case', async () => {
  let t = 0;
  const reads: string[] = [];
  const s = pullStates(undefined, () => t);
  await s.read(fake(reads), 'a', 'b', 1);
  t = 9 * 60_000;
  assert.equal((await s.read(fake(reads), 'A', 'B', 1))?.html_url, 'https://github.com/A/B/pull/1');
  assert.equal(reads.length, 1);
  t = 11 * 60_000;
  await s.read(fake(reads), 'a', 'b', 1);
  assert.equal(reads.length, 2);
});

test('a closed PR is final: never read again', async () => {
  let t = 0;
  const reads: string[] = [];
  const s = pullStates(undefined, () => t);
  s.write('A/B', { ...open, number: 1, state: 'closed', merged: true, merged_at: 'm', closed_at: 'm' } as never, 'closed');
  t = 29 * DAY;
  assert.equal((await s.read(fake(reads), 'a', 'b', 1))?.merged, true);
  assert.equal(reads.length, 0);
});

test('every write that changes the PR publishes one event; an unchanged re-read publishes none', async () => {
  let t = 0;
  const s = pullStates(undefined, () => t);
  const events = watch(s);
  const gh = fake([]);
  await s.read(gh, 'a', 'b', 1);
  t += 11 * 60_000;
  await s.read(gh, 'a', 'b', 1);
  assert.deepEqual(events.map((e) => e.action), ['opened'], 'the unchanged re-read is silent');
  gh.set({ title: 'renamed' });
  t += 11 * 60_000;
  await s.read(gh, 'a', 'b', 1);
  gh.set({ state: 'closed', merged: true, merged_at: 'm', closed_at: 'm' });
  t += 11 * 60_000;
  await s.read(gh, 'a', 'b', 1);
  s.write('A/B', { ...open, state: 'open', title: 'renamed', closed_at: null } as never, 'reopened');
  s.write('A/B', { ...open, state: 'open', title: 'renamed', closed_at: null } as never, 'reopened');
  assert.deepEqual(events.map((e) => [e.action, e.pull_request.state, e.pull_request.merged]), [['opened', 'open', false], ['edited', 'open', false], ['closed', 'closed', true], ['reopened', 'open', false]]);
  assert.deepEqual(events[2].repository, { full_name: 'A/B' });
});

test('closed entries older than 30 days are dropped on save, open ones stay; a pre-mirror entry is read again', async () => {
  const file = join(mkdtempSync(join(tmpdir(), 'pulls-')), 'p.json');
  let t = 0;
  const s = pullStates(file, () => t);
  s.write('a/b', { ...open, number: 1, state: 'closed' } as never);
  s.write('a/b', { ...open, number: 2 } as never);
  t = 31 * DAY;
  s.write('a/b', { ...open, number: 3, state: 'closed' } as never);
  assert.deepEqual(Object.keys(JSON.parse(readFileSync(file, 'utf8'))).sort(), ['a/b#2', 'a/b#3']);
  const old = join(mkdtempSync(join(tmpdir(), 'pulls-')), 'p.json');
  writeFileSync(old, JSON.stringify({ 'a/b#1': { state: 'merged', at: 'x', checked: t } }));
  const reads: string[] = [];
  await pullStates(old, () => t).read(fake(reads), 'a', 'b', 1);
  assert.equal(reads.length, 1);
});
