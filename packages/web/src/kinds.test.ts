import { expect, test } from 'vitest';
import type { Failure, Session } from './api.ts';
import { look, sessionTitle } from './kinds.ts';
import { rowOf } from './sessions.ts';
import { sess } from './test-harness.ts';

const s = (key: string, over: Partial<Session> = {}) => sess(key, over) as Session;
const row = (key: string, over: Partial<Session> = {}) => rowOf(s(key, over), false);
const ended = (outcome: NonNullable<Session['lastRun']>['outcome'], failedSteps: Failure[] = []) => ({ lastRun: { outcome, endedAt: '', failedSteps } });
const text = (x: ReturnType<typeof sessionTitle>) => [x.repo, x.number, x.text].filter(Boolean).join(' ');

test('a review is #n plus the PR title without its conventional-commit prefix, cut at a word', () => {
  const pr = sessionTitle(s('github:fagerbergj/quack#1917', { title: 'fix(v1): sleeper lineup card reports slot swaps as changes' }));
  expect(text(pr)).toBe('#1917 Sleeper lineup card reports slot swaps…');
  expect(pr.full).toBe('Sleeper lineup card reports slot swaps as changes');
  expect(text(sessionTitle(s('github:fagerbergj/quack#1913', { title: 'fix!: PDF route survives a failed export' })))).toBe('#1913 PDF route survives a failed export');
});

test('a review opened before titles were recorded reads the PR title after "Title: "', () => {
  const old = 'Review pull request fagerbergj/quack#1918 (label quack:review). Title: feat(v1): Inbox and Artifacts screens on the core';
  expect(text(sessionTitle(s('github:fagerbergj/quack#1918', { title: old })))).toBe('#1918 Inbox and Artifacts screens on the core');
  expect(text(sessionTitle(s('github:fagerbergj/quack#1919', { title: 'Review pull request fagerbergj/quack#1919 (label quack:review).' })))).toBe('#1919 quack');
});

test('the repo shows as a prefix only when the loaded reviews span more than one repo', () => {
  const one = sessionTitle(s('github:acme/tool#5', { title: 'feat: add retries' }));
  expect(text(one)).toBe('#5 Add retries');
  const many = sessionTitle({ ...s('github:acme/tool#5', { title: 'feat: add retries' }), multiRepo: true });
  expect(text(many)).toBe('tool #5 Add retries');
});

test('sleeper jobs read "Week 8 lineup" and "Week 8 waivers"', () => {
  expect(sessionTitle(s('sleeper:1356407594683482112:8:lineup')).text).toBe('Week 8 lineup');
  expect(sessionTitle(s('sleeper:1356407594683482112:8:waivers')).text).toBe('Week 8 waivers');
});

test('research keeps the question up to its first ?, comma or period, else a word cut', () => {
  const r = (title: string) => sessionTitle(s('ui:r', { agent: 'researcher', title })).text;
  expect(r('What changed in the Langfuse v4 public ingestion API? Cite sources.')).toBe('What changed in the Langfuse v4 public…');
  expect(r('Is DFlash faster? Compare it')).toBe('Is DFlash faster?');
  expect(r('Speculative decoding for Qwen3, on 4x3090')).toBe('Speculative decoding for Qwen3');
});

test('reMarkable and other sessions cut long names at about 40 characters', () => {
  expect(sessionTitle(s('remarkable:d1', { title: 'Campaign Notes' })).text).toBe('Campaign Notes');
  expect(sessionTitle(s('ui:x', { agent: 'coder', title: 'Refactor the distributor sweep so that listing never pings' })).text).toBe('Refactor the distributor sweep so that…');
});

test('an idle row reads how its last run ended, from the catalog row alone', () => {
  expect(look(row('remarkable:d1', ended('succeeded'))).line).toBe('Transcribed');
  expect(look(row('remarkable:d1')).line).toBe('Not transcribed yet');
  expect(look(row('remarkable:d1', ended('aborted'))).line).toBe('Stopped');
  expect(look(row('remarkable:d1', { state: 'running', ...ended('succeeded') })).line).toBe('Transcribing');
});

test('a failed last run reads its step until a new run works on it; pending asks read as waiting', () => {
  const failed = ended('failed', [{ step: 'ocr', error: 'timeout\nstack' }]);
  expect(look(row('remarkable:d1', failed))).toMatchObject({ line: 'Run failed at ocr · timeout', tone: 'bad' });
  expect(look(row('remarkable:d1', { state: 'running', ...failed })).line).toBe('Transcribing');
  expect(look(row('remarkable:d1', { pendingAsks: 2 }))).toMatchObject({ line: '2 waiting on you', tone: 'warn' });
  expect(look(row('remarkable:d1', { pendingAsks: 1 })).line).toBe('Waiting on you');
});

test('a failure older than the newest activity or ask gives way to waiting; one that is the latest stays red', () => {
  const failed = { lastRun: { outcome: 'failed' as const, endedAt: '2026-11-23T01:56:29Z', failedSteps: [{ step: 'model', error: 'the engine stopped' }] } };
  const asked = row('contracts:c1', { ...failed, lastActivity: '2026-11-25T04:01:24Z', pendingAsks: 1, oldestAskAt: '2026-11-25T04:05:14Z' });
  expect(look(asked)).toMatchObject({ line: 'Waiting on you', tone: 'warn' });
  expect(look(row('contracts:c1', { ...failed, lastActivity: '2026-11-23T01:56:29Z', pendingAsks: 1, oldestAskAt: '2026-11-23T01:50:00Z' })).tone).toBe('bad');
});

test('a session whose newest run stopped without an end reads Interrupted, not its never-ran words', () => {
  const cut = look(row('remarkable:d1', { lastActivity: '2026-11-02T01:00:00.000Z' }));
  expect(cut).toMatchObject({ line: 'Interrupted', tone: 'muted', why: 'The last run stopped before it finished; it resumes on the next message' });
  expect(look(row('ui:x', { agent: 'coder', lastActivity: '2026-11-02T01:00:00.000Z' })).line).toBe('Interrupted');
  expect(look(row('remarkable:d1')).line).toBe('Not transcribed yet');
  expect(look(row('remarkable:d1', { state: 'running', lastActivity: '2026-11-02T01:00:00.000Z' })).line).toBe('Transcribing');
});

test('a Home conversation reads Chat or Working, never the agent name', () => {
  expect(look(row('home:main', { agent: 'quack', ...ended('succeeded') })).line).toBe('Chat');
  expect(look(row('home:main', { agent: 'quack', state: 'running' })).line).toBe('Working');
});

test('a Home conversation is named Home whatever title it was stored with', () => {
  expect(look(row('home:main', { agent: 'quack', title: 'Plan the week' })).title).toBe('Home');
});
