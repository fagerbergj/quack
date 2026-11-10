import assert from 'node:assert/strict';
import { Agent, createServer, request } from 'node:http';
import type { AddressInfo } from 'node:net';
import { test } from 'node:test';
import { inProcessFetch, invocationRequest, safeEqual, serviceHttp, submitToSession } from '../src/index.ts';

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

test('an oversize body leaves the kept-alive connection usable for the next request', async () => {
  const s = await serve({ token: 't', maxBody: 8 });
  // One socket, so the second request must reuse the one that carried the oversize body.
  const agent = new Agent({ keepAlive: true, maxSockets: 1 });
  const post = (body: string) => new Promise<{ status?: number; text: string; socket: unknown }>((resolve, reject) => {
    const req = request(s.url, { method: 'POST', agent, headers: { authorization: 'Bearer t' } }, (res) => {
      let text = '';
      res.on('data', (c) => (text += c)).on('end', () => resolve({ status: res.statusCode, text, socket: req.socket }));
    }).on('error', reject);
    req.end(body);
  });
  try {
    const big = await post(`"${'x'.repeat(4 << 20)}"`);
    const next = await post('{"a":1}');
    assert.deepEqual([big.status, next.status, next.text, next.socket === big.socket], [400, 200, '{"b":{"a":1}}', true]);
  } finally {
    agent.destroy();
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

test('inProcessFetch rejects an aborted call while the handler still holds it', async () => {
  const held = inProcessFetch(() => new Promise(() => {}));
  const ctl = new AbortController();
  const call = held('http://app/wait', { method: 'POST', body: '{}', signal: ctl.signal });
  ctl.abort();
  await assert.rejects(call, { name: 'AbortError' });
  await assert.rejects(held('http://app/wait', { signal: AbortSignal.abort() }), { name: 'AbortError' });
});

test('a handler that answers after the abort neither resolves the call nor throws', async () => {
  let ended!: () => void;
  const done = new Promise<void>((r) => (ended = r));
  const late = inProcessFetch((_req, res) => void setTimeout(() => (res.writeHead(200), res.end('{}'), ended()), 10));
  const ctl = new AbortController();
  const call = late('http://app/wait', { signal: ctl.signal });
  ctl.abort();
  await assert.rejects(call, { name: 'AbortError' });
  await done;
});
