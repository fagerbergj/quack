import assert from 'node:assert/strict';
import { test } from 'node:test';
import { defineContract } from '../src/action.ts';

test('define_contract starts the contracts agent on the description in a session of its own', async () => {
  const sent: Array<{ url: string; body: Record<string, string> }> = [];
  const fakeFetch = (async (url: string, init: RequestInit) => {
    sent.push({ url, body: JSON.parse(String(init.body)) });
    return new Response('{}', { status: 202 });
  }) as unknown as typeof fetch;
  const started = await defineContract.start({ description: 'Define GET /x   for the page' }, { distributorUrl: 'http://d', fetch: fakeFetch, env: {} });
  assert.match(started.session, /^contracts:[0-9a-f-]{36}$/);
  assert.equal(started.title, 'Define GET /x for the page');
  assert.equal(sent[0].url, `http://d/sessions/${encodeURIComponent(started.session)}/invocations?agent=contracts`);
  assert.equal(sent[0].body.input, 'Define GET /x   for the page');
  await assert.rejects(defineContract.start({ description: 'q' }, { distributorUrl: 'http://d', fetch: (async () => new Response('{}', { status: 500 })) as unknown as typeof fetch, env: {} }), /HTTP 500/);
});
