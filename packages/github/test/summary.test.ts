import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { summarizeReview } from '../src/review.ts';

const fixture = (n: string) => readFileSync(new URL(`../../web/src/extensions/fixtures/${n}.md`, import.meta.url), 'utf8');

test('a review summarizes as its verdict and finding counts by severity', () => {
  assert.equal(summarizeReview(fixture('review-approve')), 'Approve · 1 nit');
  assert.equal(summarizeReview('## Takeaway\nx\n\n## Verdict\nrequest_changes\n\n## Findings\n- blocking: a.ts:1 — a\n- nit: b.ts:2 — b\n- nit: c.ts:3 — c\n'), 'Request changes · 1 blocking, 2 nits');
  assert.equal(summarizeReview('## Takeaway\nx\n\n## Verdict\napprove\n\n## Findings\n'), 'Approve · no findings');
});
