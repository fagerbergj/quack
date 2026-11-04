import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import { appsProxy, parseApps } from '../src/apps.ts';

const closers: Array<() => void> = [];
after(() => closers.forEach((c) => c()));

async function start(handler: Parameters<typeof createServer>[1]) {
  const server = createServer(handler).listen(0);
  await new Promise((r) => server.once('listening', r));
  closers.push(() => server.close());
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

test('the proxy forwards method, query and body, swaps the authorization, and relays the status', async () => {
  const seen: Array<{ method?: string; url?: string; auth?: string; body: string }> = [];
  const upstream = await start(async (req, res) => {
    let body = '';
    for await (const c of req) body += c;
    seen.push({ method: req.method, url: req.url, auth: req.headers.authorization, body });
    res.writeHead(418, { 'content-type': 'application/json' }).end('{"ok":1}');
  });
  const proxy = appsProxy({ demo: { url: upstream, tokenEnv: 'DEMO_TOKEN' } }, { DEMO_TOKEN: 'secret' });
  const app = await start(async (req, res) => { if (!(await proxy(req, res))) res.writeHead(404).end('other'); });

  const r = await fetch(`${app}/apps/demo/a/b?week=8`, { method: 'POST', headers: { authorization: 'Bearer from-browser', 'content-type': 'application/json' }, body: '{"x":1}' });
  assert.deepEqual([r.status, await r.json()], [418, { ok: 1 }]);
  await fetch(`${app}/apps/demo`);
  assert.deepEqual(seen, [
    { method: 'POST', url: '/apps/demo/a/b?week=8', auth: 'Bearer secret', body: '{"x":1}' },
    { method: 'GET', url: '/apps/demo', auth: 'Bearer secret', body: '' },
  ]);
});

test('an unknown app is a 404, a non-app path is left alone, and an unreachable service is a 502', async () => {
  const proxy = appsProxy({ down: { url: 'http://127.0.0.1:1', tokenEnv: 'T' } }, { T: 't' });
  const app = await start(async (req, res) => { if (!(await proxy(req, res))) res.writeHead(404).end('other'); });
  assert.equal((await fetch(`${app}/apps/nope/x`)).status, 404);
  assert.equal((await fetch(`${app}/apps/constructor`)).status, 404);
  assert.equal(await (await fetch(`${app}/sessions`)).text(), 'other');
  assert.equal((await fetch(`${app}/apps/down/x`)).status, 502);
});

test('a body over 1 MiB is a 413 and never reaches the service', async () => {
  let reached = 0;
  const upstream = await start((_req, res) => (reached++, res.end('{}')));
  const proxy = appsProxy({ demo: { url: upstream, tokenEnv: 'T' } }, { T: 't' });
  const app = await start(async (req, res) => { if (!(await proxy(req, res))) res.writeHead(404).end(); });
  const r = await fetch(`${app}/apps/demo/x`, { method: 'POST', body: Buffer.alloc((1 << 20) + 1) });
  assert.equal(r.status, 413);
  assert.equal(reached, 0);
});

test('an upstream that fails mid-body leaves the client with a broken response, not a second JSON body', async () => {
  const upstream = await start((_req, res) => {
    res.writeHead(200, { 'content-type': 'application/json', 'content-length': '100' });
    res.write('{"partial":', () => res.destroy());
  });
  const proxy = appsProxy({ demo: { url: upstream, tokenEnv: 'T' } }, { T: 't' });
  const app = await start(async (req, res) => { if (!(await proxy(req, res))) res.writeHead(404).end(); });
  await assert.rejects(async () => (await fetch(`${app}/apps/demo/x`)).text());
});

test('an unreachable service is a 502 problem+json', async () => {
  const proxy = appsProxy({ demo: { url: 'http://127.0.0.1:1', tokenEnv: 'T' } }, { T: 't' });
  const app = await start(async (req, res) => { if (!(await proxy(req, res))) res.writeHead(404).end(); });
  const r = await fetch(`${app}/apps/demo/x`);
  assert.equal(r.status, 502);
  assert.equal(r.headers.get('content-type'), 'application/problem+json');
  const body = (await r.json()) as { title: string; status: number; detail: string; error: string };
  assert.deepEqual([body.title, body.status, body.error === body.detail && body.detail.length > 0], ['Bad Gateway', 502, true]);
});

test('QUACK_APPS is optional and refuses an entry whose token env is unset', () => {
  assert.deepEqual(parseApps(undefined), {});
  assert.deepEqual(parseApps('{"a":{"url":"http://a","tokenEnv":"T"}}', { T: 'x' }), { a: { url: 'http://a', tokenEnv: 'T' } });
  assert.throws(() => parseApps('{"a":{"url":"http://a","tokenEnv":"T"}}', {}), /QUACK_APPS.a/);
});
