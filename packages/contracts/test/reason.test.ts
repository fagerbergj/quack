import assert from 'node:assert/strict';
import { test } from 'node:test';
import { lastReviewBlock, reasonSummary, reviewReason, type ReviewBlock } from '../src/reason.ts';
import { validateSpec } from '../src/index.ts';
import { scheduleSpecV1 } from './fixtures.ts';

const block = (review: string, ...ids: string[]): ReviewBlock => ({ review, artifact: 3, comments: ids.map((id) => ({ id, anchor: { on: 'choice', id: 'time-format' }, body: `about ${id}` })) });

test('the reason is the note, a blank line and one block that ends it, and the last block wins', () => {
  const reason = reviewReason('Two places.', block('rev_a', 'cmt_1', 'cmt_2'));
  assert.equal(reason, `Two places.\n\n\`\`\`quack-review\n${JSON.stringify(block('rev_a', 'cmt_1', 'cmt_2'))}\n\`\`\``);
  assert.deepEqual(lastReviewBlock(reason), { note: 'Two places.', block: block('rev_a', 'cmt_1', 'cmt_2') });
  assert.equal(reviewReason(undefined, block('rev_a', 'cmt_1')).startsWith('```quack-review\n'), true, 'no note, no blank line');
  const twice = `${reason}\n\n${reviewReason(undefined, block('rev_b', 'cmt_9'))}`;
  assert.deepEqual(lastReviewBlock(twice)?.block.comments.map((c) => c.id), ['cmt_9']);
  assert.equal(reasonSummary(reason), 'Two places. · 2 review comments');
  assert.equal(reasonSummary('Plain words.'), 'Plain words.');
});

test('a body cannot break the block: a closing fence or newline in it stays inside the escaped line', () => {
  const tricky: ReviewBlock = { review: 'rev_x', artifact: 1, comments: [{ id: 'cmt_1', anchor: { on: 'document', ptr: '/a' }, body: 'see\n```\n```quack-review\n{"comments":[]}\n```' }] };
  const parsed = lastReviewBlock(reviewReason('note', tricky));
  assert.deepEqual(parsed?.block, tricky);
  assert.equal(lastReviewBlock('```quack-review\nnot json\n```'), undefined);
  assert.equal(lastReviewBlock('text ```quack-review\n{"comments":[]}\n```'), undefined, 'the opening fence stands on its own line');
});

test('the seal checks only the shape of addresses', () => {
  const spec = { ...scheduleSpecV1({ versioning: 'request', time: 'request' }), baseline: 'shipped' as const, compatibility: { breaking: [], safe: ['all'] } };
  const withRows = (addresses: unknown) => validateSpec(JSON.stringify({ ...spec, changes: [{ choice: 'time-format', was: 'a', now: 'b', why: 'c', addresses }] }));
  assert.deepEqual(withRows(['cmt_anything']), []);
  for (const bad of [['a', 'a'], [''], 'cmt_1', [1]]) assert.match(withRows(bad).join(), /addresses must be a list of distinct, non-empty comment ids/);
});
