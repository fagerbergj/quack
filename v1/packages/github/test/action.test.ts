import assert from 'node:assert/strict';
import { test } from 'node:test';
import { reviewPullRequest } from '../src/action.ts';

const sent: Array<{ url: string; body: Record<string, string> }> = [];
const fakeFetch = (async (url: string, init: RequestInit) => {
  sent.push({ url, body: JSON.parse(String(init.body)) });
  return new Response('{}', { status: 202 });
}) as unknown as typeof fetch;
const ctx = { distributorUrl: 'http://d', fetch: fakeFetch, env: {} };

test('review_pull_request starts the reviewer under the label\'s session key', async () => {
  const started = await reviewPullRequest.start({ url: 'https://github.com/acme/widgets/pull/7' }, ctx);
  assert.deepEqual(started, { session: 'github:acme/widgets#7', title: 'Review acme/widgets#7' });
  assert.equal(sent[0].url, 'http://d/sessions/github%3Aacme%2Fwidgets%237/invocations?agent=reviewer');
  assert.match(sent[0].body.input, /^Review pull request acme\/widgets#7/);
  assert.equal(sent[0].body.title, 'Review acme/widgets#7');
});

test('review_pull_request refuses anything but a github.com pull request URL', async () => {
  for (const url of ['https://example.com/acme/widgets/pull/7', 'https://github.com/acme/widgets/issues/7', 'nonsense']) await assert.rejects(reviewPullRequest.start({ url }, ctx));
});

test('review_pull_request reports a distributor refusal', async () => {
  const refusing = { ...ctx, fetch: (async () => new Response('{}', { status: 502 })) as unknown as typeof fetch };
  await assert.rejects(reviewPullRequest.start({ url: 'https://github.com/acme/widgets/pull/7' }, refusing), /HTTP 502/);
});
