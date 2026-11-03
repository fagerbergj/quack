import assert from 'node:assert/strict';
import { test } from 'node:test';
import { researchQuestion } from '../src/action.ts';

test('research_question starts the researcher on the question in a session of its own', async () => {
  const sent: Array<{ url: string; body: Record<string, string> }> = [];
  const fakeFetch = (async (url: string, init: RequestInit) => {
    sent.push({ url, body: JSON.parse(String(init.body)) });
    return new Response('{}', { status: 202 });
  }) as unknown as typeof fetch;
  const started = await researchQuestion.start({ question: 'Why is   the sky blue?' }, { distributorUrl: 'http://d', fetch: fakeFetch, env: {} });
  assert.match(started.session, /^research:[0-9a-f-]{36}$/);
  assert.equal(started.title, 'Why is the sky blue?');
  assert.equal(sent[0].url, `http://d/sessions/${encodeURIComponent(started.session)}/invocations?agent=researcher`);
  assert.equal(sent[0].body.input, 'Why is   the sky blue?');
  await assert.rejects(researchQuestion.start({ question: 'q' }, { distributorUrl: 'http://d', fetch: (async () => new Response('{}', { status: 500 })) as unknown as typeof fetch, env: {} }), /HTTP 500/);
});
