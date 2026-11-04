import assert from 'node:assert/strict';
import { createServer, request } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import type { App, AppContext, DeploymentFile, ExtensionManifest } from '@quack/sdk';
import { appsGateway, mountApps } from '../src/apps.ts';
import { credentialBroker } from '../src/credentials.ts';

const closers: Array<() => void> = [];
after(() => closers.forEach((c) => c()));

const seen: Array<{ method?: string; url?: string; auth?: string; body: string }> = [];
const demo: App = {
  pages: [{ method: 'GET', path: '/' }, { method: 'POST', path: '/hook' }, { method: 'DELETE', path: /^\/item-\d+$/ }],
  async handle(req, res) {
    let body = '';
    for await (const c of req) body += c;
    seen.push({ method: req.method, url: req.url, auth: req.headers.authorization, body });
    res.writeHead(418, { 'content-type': 'application/json' }).end('{"ok":1}');
  },
};
const broker = credentialBroker();
const granted = new Set(['demo']);
const gateway = appsGateway({ demo, other: { ...demo, pages: [] } }, broker.sessionOf, async (key) => (key === 'allowed' ? granted : new Set()));

const server = createServer(async (req, res) => { if (!(await gateway(req, res))) res.writeHead(404).end('other'); }).listen(0);
await new Promise((r) => server.once('listening', r));
closers.push(() => server.close());
const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
const allowed = broker.mint('allowed');
const ungranted = broker.mint('ungranted');
const as = (token: string) => ({ authorization: `Bearer ${token}` });

test('a granted session reaches the app with the path relative to its mount, method, query and body intact', async () => {
  seen.length = 0;
  const r = await fetch(`${base}/apps/demo/a/b?week=8`, { method: 'POST', headers: { ...as(allowed), 'content-type': 'application/json' }, body: '{"x":1}' });
  assert.deepEqual([r.status, await r.json()], [418, { ok: 1 }]);
  await fetch(`${base}/apps/demo`, { headers: as(allowed) });
  assert.deepEqual(seen.map((s) => [s.method, s.url, s.body]), [['POST', '/a/b?week=8', '{"x":1}'], ['GET', '/', '']]);
});

test('a session whose agent does not declare the app gets a 403 problem, and a bad or revoked token a 401', async () => {
  const forbidden = await fetch(`${base}/apps/demo/x`, { headers: as(ungranted) });
  assert.equal(forbidden.status, 403);
  assert.equal(forbidden.headers.get('content-type'), 'application/problem+json');
  assert.equal(((await forbidden.json()) as { detail: string }).detail, 'agent has no grant for this app');
  assert.equal((await fetch(`${base}/apps/demo/x`, { headers: as('nope') })).status, 401);
  const gone = broker.mint('allowed');
  broker.revoke(gone);
  assert.equal((await fetch(`${base}/apps/demo/x`, { headers: as(gone) })).status, 401);
});

test('a credential-free request reaches only declared page and ingress routes; any other route is a 401 problem', async () => {
  seen.length = 0;
  assert.equal((await fetch(`${base}/apps/demo`)).status, 418, 'a page route');
  assert.equal((await fetch(`${base}/apps/demo/hook`, { method: 'POST', body: '{}' })).status, 418, 'the webhook reaches the handler, which verifies the signature');
  assert.equal((await fetch(`${base}/apps/demo/item-7`, { method: 'DELETE' })).status, 418, 'a pattern route');
  assert.equal(seen.every((s) => s.auth === undefined), true);
  const before = seen.length;
  for (const [method, path] of [['GET', '/private'], ['POST', '/'], ['GET', '/hook'], ['DELETE', '/item-x']]) {
    const r = await fetch(`${base}/apps/demo${path}`, { method });
    assert.deepEqual([r.status, r.headers.get('content-type')], [401, 'application/problem+json'], `${method} ${path}`);
  }
  assert.equal((await fetch(`${base}/apps/other`)).status, 401, 'an app that declares no page routes has none');
  assert.equal(seen.length, before, 'none of them reached a handler');
});

test('a granted session reaches a non-page route, and a page route too; an ungranted session never does', async () => {
  assert.equal((await fetch(`${base}/apps/demo/private`, { headers: as(allowed) })).status, 418);
  assert.equal((await fetch(`${base}/apps/demo`, { headers: as(allowed) })).status, 418);
  assert.equal((await fetch(`${base}/apps/demo`, { headers: as(ungranted) })).status, 403);
});

test('an unmounted app is a 404 problem, and a non-app path is left alone', async () => {
  const r = await fetch(`${base}/apps/nope/x`, { headers: as(allowed) });
  assert.equal(r.status, 404);
  assert.equal(((await r.json()) as { detail: string }).detail, 'no such app');
  assert.equal((await fetch(`${base}/apps/constructor`)).status, 404);
  assert.equal(await (await fetch(`${base}/sessions`)).text(), 'other');
});

// fetch would resolve `..` before sending; a raw request sends the path as an attacker would.
const rawGet = (path: string) => new Promise<number>((resolve, reject) => request(base + '/', { path, headers: as(allowed) }, (r) => (r.resume(), resolve(r.statusCode!))).on('error', reject).end());

test('a path that dot-segments out of /apps/<name> is a 404 and never reaches the app', async () => {
  seen.length = 0;
  for (const path of ['/apps/demo/../../git-credential', '/apps/demo/%2e%2e/%2E%2E/schedules', '/apps/demo/../other/x', '/apps/demo/..']) {
    assert.equal(await rawGet(path), 404, path);
  }
  assert.equal(await rawGet('/apps/demo/a/../b'), 418);
  assert.deepEqual(seen.map((s) => s.url), ['/b']);
});

test('only extensions the deployment lists are mounted, and a listed name nothing mounts is refused', async () => {
  const built: string[] = [];
  const manifests: ExtensionManifest[] = ['a', 'b'].map((name) => ({ name, pages: [{ method: 'GET', path: '/' }], app: (ctx) => (built.push(`${name}:${ctx.dbPath}`), { ...demo }) }));
  const file: DeploymentFile = { extensions: { a: {} } };
  const apps = await mountApps(file, manifests, { distributorUrl: 'http://d', distributor: () => {}, env: { QUACK_DATA_DIR: '/data' } });
  assert.deepEqual([Object.keys(apps), built, apps.a.pages], [['a'], ['a:/data/a.db'], [{ method: 'GET', path: '/' }]]);
  await assert.rejects(mountApps({ extensions: { c: {} } }, manifests, { distributorUrl: 'http://d', distributor: () => {}, env: { QUACK_DATA_DIR: '/data' } }), /extensions\.c: no extension named c mounts an app/);
});

test('an app reaches the distributor and another app in process, with no socket or credential', async () => {
  const peer: App = { handle: async (req, res) => void res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ seen: req.url, auth: req.headers.authorization ?? null })) };
  let ctx: AppContext | undefined;
  const manifests: ExtensionManifest[] = [{ name: 'a', app: (c) => ((ctx = c), peer) }, { name: 'b', app: () => peer }];
  await mountApps({ extensions: { a: {}, b: {} } }, manifests, { distributorUrl: 'http://d', distributor: (req, res) => void res.writeHead(202).end(`${req.method} ${req.url}`), env: { QUACK_DATA_DIR: '/data' } });
  const sent = await ctx!.fetch('http://d/sessions/k/invocations?agent=x', { method: 'POST', body: '{}' });
  assert.deepEqual([sent.status, await sent.text()], [202, 'POST /sessions/k/invocations?agent=x']);
  assert.deepEqual(await (await ctx!.peerFetch('b')('http://d/items?x=1')).json(), { seen: '/items?x=1', auth: null });
  assert.equal((await ctx!.peerFetch('missing')('http://d/')).status, 404);
});
