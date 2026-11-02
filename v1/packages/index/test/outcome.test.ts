import assert from 'node:assert/strict';
import { test } from 'node:test';
import { outcomeOf } from '../src/facts.ts';

const review = (verdict: string) => ({ content: `## Takeaway\nFine.\n\n## Verdict\n${verdict}\n\n## Findings\nNone.\n` });

test('the outcome is the verdict section first word, lowercased, as the review is posted', () => {
  assert.equal(outcomeOf(review('approve')), 'approve');
  assert.equal(outcomeOf(review('Approve')), 'approve');
  assert.equal(outcomeOf(review('approve (no blocking findings)')), 'approve');
  assert.equal(outcomeOf(review('request_changes')), 'request_changes');
  assert.equal(outcomeOf({ content: '# Review\nfine\n' }), undefined);
});
