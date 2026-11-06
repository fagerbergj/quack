import assert from 'node:assert/strict';
import { generateKeyPairSync } from 'node:crypto';
import { test } from 'node:test';
import { githubApp, GithubError, RateLimited } from '../src/app.ts';

const { privateKey } = generateKeyPairSync('rsa', { modulusLength: 2048, privateKeyEncoding: { type: 'pkcs8', format: 'pem' }, publicKeyEncoding: { type: 'spki', format: 'pem' } });

// A GitHub that answers from a script, one response per request, and records what each request carried.
function scripted(responses: Array<() => Response | Promise<Response>>) {
  const seen: RequestInit[] = [];
  const doFetch = (async (_url: string, init: RequestInit) => (seen.push(init), responses.shift()!())) as unknown as typeof fetch;
  const sleeps: number[] = [];
  const app = githubApp({ appId: '1', privateKey, apiUrl: 'http://gh', sleep: async (ms) => { sleeps.push(ms); } }, doFetch);
  return { app, seen, sleeps };
}
const json = (status: number, body: unknown, headers: Record<string, string> = {}) => () => new Response(JSON.stringify(body), { status, headers });

test('a secondary rate limit is waited out for as long as Retry-After says, then the request goes again', async () => {
  const { app, seen, sleeps } = scripted([json(429, { message: 'slow down' }, { 'retry-after': '2' }), json(200, { ok: 1 })]);
  assert.deepEqual((await app.asApp('POST', '/app/hook/deliveries/1/attempts')).json, { ok: 1 });
  assert.deepEqual(sleeps, [2000]);
  assert.equal(seen.length, 2);
});

test('a 403 that says rate limit waits a minute; a 403 that does not is a refusal, never retried', async () => {
  const limited = scripted([json(403, { message: 'You have exceeded a secondary rate limit' }), json(200, {})]);
  await limited.app.asApp('GET', '/app');
  assert.deepEqual(limited.sleeps, [60_000]);
  const denied = scripted([json(403, { message: 'Resource not accessible by integration' })]);
  await assert.rejects(denied.app.asApp('GET', '/app'), (err) => err instanceof GithubError && !(err instanceof RateLimited) && err.status === 403);
  assert.deepEqual(denied.sleeps, []);
});

test('a primary limit whose reset is far off is handed back with its wait, not slept through', async () => {
  const reset = Math.floor(Date.now() / 1000) + 3600;
  const { app, sleeps } = scripted([json(403, { message: 'API rate limit exceeded' }, { 'x-ratelimit-remaining': '0', 'x-ratelimit-reset': String(reset) })]);
  await assert.rejects(app.asApp('GET', '/app'), (err) => err instanceof RateLimited && err.retryAfterMs > 3_000_000);
  assert.deepEqual(sleeps, []);
});

test('retries are bounded: a limit that persists ends as RateLimited after three tries', async () => {
  const { app, seen, sleeps } = scripted(Array.from({ length: 5 }, () => json(429, {}, { 'retry-after': '1' })));
  await assert.rejects(app.asApp('GET', '/app'), RateLimited);
  assert.equal(seen.length, 3);
  assert.deepEqual(sleeps, [1000, 1000]);
});

test('every request carries a timeout, and one that times out is an outage', async () => {
  const { app, seen } = scripted([() => Promise.reject(new DOMException('The operation was aborted due to timeout', 'TimeoutError'))]);
  await assert.rejects(app.asApp('GET', '/app'), (err) => err instanceof GithubError && err.status === 502 && /timed out/.test(err.message));
  assert.ok(seen[0].signal instanceof AbortSignal);
});

test('no more than the cap of requests are in flight at once', async () => {
  let inFlight = 0;
  let most = 0;
  const gates: Array<() => void> = [];
  const doFetch = (async () => {
    most = Math.max(most, ++inFlight);
    await new Promise<void>((r) => gates.push(r));
    inFlight--;
    return new Response('{}', { status: 200 });
  }) as unknown as typeof fetch;
  const app = githubApp({ appId: '1', privateKey, apiUrl: 'http://gh', maxConcurrent: 3 }, doFetch);
  const all = Promise.all(Array.from({ length: 10 }, (_, i) => app.asApp('GET', `/x/${i}`)));
  // Release one request at a time; each release lets exactly one waiter in.
  for (let released = 0; released < 10; released++) {
    while (!gates.length) await new Promise((r) => setImmediate(r));
    gates.shift()!();
  }
  await all;
  assert.equal(most, 3);
});
