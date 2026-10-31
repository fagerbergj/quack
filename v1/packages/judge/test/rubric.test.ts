import assert from 'node:assert/strict';
import { test } from 'node:test';
import { feedback, score, type CheckResult, type Rubric } from '../src/index.ts';

const rubric: Rubric = { name: 'r', version: 1, threshold: 0.8, criteria: [] };
const r = (id: string, weight: number | 'gate', met: number | null, units = 1, notes: string[] = []): CheckResult => ({ criterion: 'c', id, weight, met, units, resolvers: {}, notes });

test('scores earned over possible points, and pitfalls subtract', () => {
  assert.deepEqual(score(rubric, [r('a', 4, 1), r('b', 2, 0.5), r('c', 2, 1)]), { score: 7 / 8, gatesFailed: [], passed: true });
  assert.equal(score(rubric, [r('a', 4, 1), r('b', 4, 1), r('pit', -4, 1)]).score, 0.5);
  assert.equal(score(rubric, [r('a', 2, 0), r('pit', -4, 1)]).score, 0, 'never below zero');
});

test('a failed gate fails the version whatever the score', () => {
  const s = score(rubric, [r('gate', 'gate', 0.75, 4), r('a', 4, 1)]);
  assert.equal(s.score, 1);
  assert.deepEqual(s.gatesFailed, ['c.gate']);
  assert.equal(s.passed, false);
});

test('checks that apply to no unit count toward neither side', () => {
  assert.equal(score(rubric, [r('a', 3, 1), r('per-finding', 4, null, 0), r('gate', 'gate', null, 0)]).score, 1);
  assert.equal(score(rubric, []).passed, true);
});

test('the threshold decides a pass when no gate fails', () => {
  assert.equal(score(rubric, [r('a', 4, 0.75)]).passed, false);
  assert.equal(score(rubric, [r('a', 4, 0.8)]).passed, true);
});

test('feedback names failed gates, missed checks with shares, and pitfalls present', () => {
  const lines = feedback([r('cites', 'gate', 0.5, 2, ['f2: no line cited']), r('why', 3, 1), r('show', 2, 0.5, 4), r('pref', -3, 0), r('contradicted', -4, 0.5, 2)]);
  assert.deepEqual(lines, ['gate c.cites (1 of 2): f2: no line cited', 'missed c.show (2 of 4)', 'pitfall c.contradicted (1 of 2)']);
});
