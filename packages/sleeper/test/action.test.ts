import assert from 'node:assert/strict';
import { test } from 'node:test';
import { runJob } from '../src/action.ts';

const env = { QUACK_INDEX_URL: 'http://d', QUACK_INDEX_TOKEN: 'secret' };

test('run_job posts to the app\'s on-demand endpoint with the launch credential and returns its session', async () => {
  const seen: Array<{ url: string; method?: string; auth: string }> = [];
  const fetchStub = (async (url: string, init: RequestInit) => {
    seen.push({ url, method: init.method, auth: (init.headers as Record<string, string>).authorization });
    return new Response(JSON.stringify({ sessionKey: 'sleeper:L 1:9:waivers', status: 202 }), { status: 202 });
  }) as unknown as typeof fetch;
  const started = await runJob.start({ job: 'waivers', week: 9 }, { distributorUrl: 'http://d', fetch: fetchStub, env });
  assert.deepEqual(started, { session: 'sleeper:L 1:9:waivers', title: 'Sleeper waivers, week 9' });
  assert.deepEqual(seen, [{ url: 'http://d/apps/sleeper/waivers?week=9', method: 'POST', auth: 'Bearer secret' }]);
  await runJob.start({ job: 'lineup' }, { distributorUrl: 'http://d', fetch: fetchStub, env });
  assert.equal(seen[1].url, 'http://d/apps/sleeper/lineup', 'no week runs the current one');
});

test('run_job surfaces the service\'s refusal and a missing configuration', async () => {
  const refusing = (async () => new Response(JSON.stringify({ error: 'on-demand jobs are not configured' }), { status: 501 })) as unknown as typeof fetch;
  await assert.rejects(runJob.start({ job: 'digest' }, { distributorUrl: 'http://d', fetch: refusing, env }), /not configured/);
  await assert.rejects(runJob.start({ job: 'digest' }, { distributorUrl: 'http://d', fetch: refusing, env: {} }), /QUACK_INDEX_URL and QUACK_INDEX_TOKEN are required/);
});
