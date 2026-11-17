import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { Approvals, Artifacts, type Approval, type SealedArtifact } from '@quack/sdk';
import { commentProblems } from '../src/answered.ts';
import { lastReviewBlock, reasonSummary, reviewReason, type ReviewBlock } from '../src/reason.ts';
import { recordedSpecInputs, specInputs, validateSpec, type Change } from '../src/index.ts';
import { scheduleSpecV1 } from './fixtures.ts';

const block = (review: string, ...ids: string[]): ReviewBlock => ({ review, artifact: 3, comments: ids.map((id) => ({ id, anchor: { on: 'choice', id: 'time-format' }, body: `about ${id}` })) });
const rejection = (at: number, reason: string, extra: Partial<Approval> = {}): Approval => ({ tool: 'propose_contract', args: {}, requestedAt: at - 1, verdict: { decision: 'reject', reason, by: { kind: 'person', name: 'ada' }, at }, executing: false, done: true, ...extra });
const reply = (comment: string, sealedAt: number): SealedArtifact => ({ kind: 'contract.reply', version: 1, hash: '', content: JSON.stringify({ comment, text: 'left as is' }), sealedAt, verdict: null });
const row = (...addresses: string[]): Change => ({ choice: 'time-format', was: 'a', now: 'b', why: 'c', addresses });
const unanswered = (id: string) => `comment '${id}' from the last Ask for changes is neither addressed nor answered, so name it in the addresses of the changes row that changes its anchored text, or answer it with reply_comment`;
const invented = (id: string) => `addresses names '${id}', which no Ask for changes sent to this session, so remove it`;

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

test('every id of the newest rejection must be addressed or answered; an invented id fails; a plain rejection passes', () => {
  const approvals = { a: rejection(10, reviewReason('n', block('rev_a', 'cmt_1', 'cmt_2', 'cmt_3'))) };
  const kinds = { 'contract.reply': { versions: [reply('cmt_2', 15)] } };
  assert.deepEqual(commentProblems([row('cmt_1', 'cmt_bogus')], 20, approvals, kinds), [unanswered('cmt_3'), invented('cmt_bogus')]);
  assert.deepEqual(commentProblems([row('cmt_1', 'cmt_3')], 20, approvals, kinds), []);
  assert.deepEqual(commentProblems(undefined, 20, { a: rejection(10, 'Please use ISO dates.') }, {}), [], 'a plain Ask for changes');
  assert.deepEqual(commentProblems(undefined, 20, {}, {}), [], 'never rejected');
  assert.deepEqual(commentProblems([row('cmt_1')], 20, {}, {}), [invented('cmt_1')], 'no rejection sent it');
});

test('a reply sealed after the spec does not count, nor does one from another session (it is not in this one)', () => {
  const approvals = { a: rejection(10, reviewReason(undefined, block('rev_a', 'cmt_1'))) };
  assert.deepEqual(commentProblems([], 20, approvals, { 'contract.reply': { versions: [reply('cmt_1', 21)] } }), [unanswered('cmt_1')]);
  assert.deepEqual(commentProblems([], 20, approvals, { 'contract.reply': { versions: [reply('cmt_1', 20)] } }), []);
  assert.deepEqual(commentProblems([row('cmt_other')], 20, approvals, {}), [unanswered('cmt_1'), invented('cmt_other')], 'an id another session was sent');
});

test('a withdrawn rejection is skipped, a later plain rejection drops the block, and rejections after the seal do not count', () => {
  const sent = rejection(10, reviewReason(undefined, block('rev_a', 'cmt_1')));
  assert.deepEqual(commentProblems([], 30, { a: sent, b: rejection(20, reviewReason(undefined, block('rev_b', 'cmt_2')), { withdrawn: { at: 21, reason: 'superseded', supersededBy: null } }) }, {}), [unanswered('cmt_1')], 'the withdrawn one never reached the agent');
  assert.deepEqual(commentProblems([], 30, { a: sent, b: rejection(20, 'Start over.') }, {}), [], 'a later plain rejection');
  assert.deepEqual(commentProblems([row('cmt_1')], 30, { a: sent, b: rejection(20, 'Start over.') }, {}), [], 'its ids still count as sent for addresses');
  assert.deepEqual(commentProblems([], 15, { a: sent, b: rejection(20, reviewReason(undefined, block('rev_b', 'cmt_2'))) }, {}), [unanswered('cmt_1')], 'decided after the seal');
  assert.deepEqual(commentProblems([], 9, { a: sent }, {}), []);
});

test('the seal checks only the shape of addresses', () => {
  const spec = { ...scheduleSpecV1({ versioning: 'request', time: 'request' }), baseline: 'shipped' as const, compatibility: { breaking: [], safe: ['all'] } };
  const withRows = (addresses: unknown) => validateSpec(JSON.stringify({ ...spec, changes: [{ choice: 'time-format', was: 'a', now: 'b', why: 'c', addresses }] }));
  assert.deepEqual(withRows(['cmt_anything']), []);
  for (const bad of [['a', 'a'], [''], 'cmt_1', [1]]) assert.match(withRows(bad).join(), /addresses must be a list of distinct, non-empty comment ids/);
});

const content = JSON.stringify({ ...scheduleSpecV1({ versioning: 'request', time: 'request' }), baseline: 'shipped', changes: [], compatibility: { breaking: [], safe: [] } });
const spec: SealedArtifact = { kind: 'contract.spec', version: 2, hash: '', content, sealedAt: 50, verdict: null };
const unansweredIn = (facts: string) => (JSON.parse(facts) as { unanswered: string[] }).unanswered;

test('the live path applies the seal cutoff itself, since it reads every approval', async () => {
  const approvals = { early: rejection(40, reviewReason(undefined, block('rev_a', 'cmt_1'))), late: rejection(60, reviewReason(undefined, block('rev_b', 'cmt_2'))) };
  const docs = new Map<unknown, unknown>([[Artifacts, { kinds: { 'contract.spec': { versions: [spec] } } }], [Approvals, { items: approvals }]]);
  const api = { conversationId: 'c', env: undefined, snapshot: async (def: unknown) => docs.get(def) } as never;
  assert.deepEqual(unansweredIn((await specInputs()(api, spec, ctx)).facts), [unanswered('cmt_1')]);
  const recorded = await recordedSpecInputs({ docs: { 'quack.approvals': { items: approvals } as never }, artifacts: { 'contract.spec': [spec], 'contract.reply': [reply('cmt_1', 45)] }, readFile: async () => undefined }, content);
  assert.deepEqual(unansweredIn(recorded.facts), [], 'a recording answers it with the reply sealed before the spec');
});
