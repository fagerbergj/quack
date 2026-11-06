import assert from 'node:assert/strict';
import { test } from 'node:test';
import { SUMMARY_MAX, summaryOf, type ArtifactKind } from '../src/artifacts.ts';

const kind = (summarize?: (c: string) => string): ArtifactKind => ({ kind: 'k', path: 'p', summarize });

test('a summary is one line cut to the limit', () => {
  assert.equal(summaryOf(kind((c) => `  ${c}\n\n x `), 'a'), 'a x');
  const long = summaryOf(kind(() => 'word '.repeat(40)), '')!;
  assert.ok(long.length <= SUMMARY_MAX && long.endsWith('…'));
});

test('a kind without a summarizer, with an empty one, or that throws has no summary', () => {
  assert.equal(summaryOf(kind(), 'x'), undefined);
  assert.equal(summaryOf(kind(() => ' '), 'x'), undefined);
  assert.equal(summaryOf(kind(() => { throw new Error('bad'); }), 'x'), undefined);
});
