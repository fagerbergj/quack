import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import { sleeperRoutes } from '../src/sleeper-routes.ts';

const closers: Array<() => void> = [];
after(() => closers.forEach((c) => c()));

async function start(handler: Parameters<typeof createServer>[1]) {
  const server = createServer(handler).listen(0);
  await new Promise((r) => server.once('listening', r));
  closers.push(() => server.close());
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

test('the lineup route adds the token and the configured league, and relays the answer', async () => {
  const seen: Array<{ url?: string; auth?: string; method?: string }> = [];
  const upstream = await start((req, res) => {
    seen.push({ url: req.url, auth: req.headers.authorization, method: req.method });
    res.writeHead(202, { 'content-type': 'application/json' }).end(JSON.stringify({ sessionKey: 'sleeper:L:8:lineup', status: 202 }));
  });
  const app = await start(async (req, res) => {
    if (!(await sleeperRoutes({ url: upstream, token: 'secret', league: 'L' })(req, res))) res.writeHead(404).end();
  });

  const run = await fetch(`${app}/apps/sleeper/lineup?week=8`, { method: 'POST', headers: { authorization: 'Bearer from-browser' } });
  assert.equal(run.status, 202);
  assert.deepEqual(await run.json(), { sessionKey: 'sleeper:L:8:lineup', status: 202 });
  assert.deepEqual(seen, [{ url: '/leagues/L/jobs/lineup?week=8', auth: 'Bearer secret', method: 'POST' }]);

  assert.deepEqual(await (await fetch(`${app}/apps/sleeper`)).json(), { league: 'L' });
  assert.equal((await fetch(`${app}/apps/sleeper/lineup?week=x`, { method: 'POST' })).status, 400);
  assert.equal((await fetch(`${app}/apps/sleeper/lineup`)).status, 404);
  assert.equal(seen.length, 1);
});

test('an unreachable service is a 502, not a hang', async () => {
  const app = await start(async (req, res) => {
    if (!(await sleeperRoutes({ url: 'http://127.0.0.1:1', token: 't', league: 'L' })(req, res))) res.writeHead(404).end();
  });
  assert.equal((await fetch(`${app}/apps/sleeper/lineup`, { method: 'POST' })).status, 502);
});
