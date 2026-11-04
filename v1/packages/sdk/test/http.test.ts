import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { test } from 'node:test';
import { invocationRequest, safeEqual, serviceHttp, submitToSession } from '../src/index.ts';

async function serve(opts: Parameters<typeof serviceHttp>[0]) {
  const http = serviceHttp(opts);
  const server = createServer((req, res) => {
    if (!http.authorized(req)) return void http.send(res, 401, { error: 'unauthorized' });
    http.json(req).then((b) => http.send(res, 200, { b }), (e) => http.send(res, 400, { error: (e as Error).message }));
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  return { url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, close: () => server.close() };
}

test('serviceHttp checks the bearer, parses JSON and refuses an oversize body', async () => {
  const s = await serve({ token: 't', maxBody: 8 });
  try {
    assert.equal((await fetch(s.url, { method: 'POST', body: '{}' })).status, 401);
    const ok = await fetch(s.url, { method: 'POST', headers: { authorization: 'Bearer t' }, body: '{"a":1}' });
    assert.deepEqual(await ok.json(), { b: { a: 1 } });
    const big = await fetch(s.url, { method: 'POST', headers: { authorization: 'Bearer t' }, body: '{"a":"xxxxxxxx"}' });
    assert.deepEqual(await big.json(), { error: 'body too large' });
  } finally {
    s.close();
  }
});

test('safeEqual is false for different lengths and values', () => {
  assert.equal(safeEqual('abc', 'abc'), true);
  assert.equal(safeEqual('abc', 'abd'), false);
  assert.equal(safeEqual('abc', 'ab'), false);
});

test('submitToSession encodes the key and agent and sends only the given fields', async () => {
  const seen: Array<{ url: string; body: unknown }> = [];
  const fake = (async (url: string, init: RequestInit) => (seen.push({ url, body: JSON.parse(init.body as string) }), new Response('{}'))) as unknown as typeof fetch;
  await submitToSession('http://d', { key: 'a b:1/2', agent: 'x y', input: 'hi', requestId: 'r', whenBusy: 'followUp' }, fake);
  assert.deepEqual(seen, [{ url: 'http://d/sessions/a%20b%3A1%2F2/invocations?agent=x%20y', body: { input: 'hi', requestId: 'r', whenBusy: 'followUp' } }]);
  assert.equal(invocationRequest('http://d', { key: 'k', agent: 'a', input: 'i', requestId: 'r', wait: true }).body, '{"input":"i","requestId":"r","wait":true}');
});

test('problem answers RFC 9457 JSON that still carries error', async () => {
  const server = createServer((_req, res) => serviceHttp().problem(res, 413, 'body too large'));
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  try {
    const res = await fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}`);
    assert.equal(res.headers.get('content-type'), 'application/problem+json');
    assert.deepEqual(await res.json(), { type: 'about:blank', title: 'Payload Too Large', status: 413, detail: 'body too large', error: 'body too large' });
  } finally {
    server.close();
  }
});
