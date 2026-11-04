import assert from 'node:assert/strict';
import { createServer, request } from 'node:http';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import type { App, DeploymentFile, ExtensionManifest } from '@quack/sdk';
import { appsGateway, mountApps } from '../src/apps.ts';
import { credentialBroker } from '../src/credentials.ts';

const closers: Array<() => void> = [];
after(() => closers.forEach((c) => c()));

const seen: Array<{ method?: string; url?: string; auth?: string; body: string }> = [];
const demo: App = {
  async handle(req, res) {
    let body = '';
    for await (const c of req) body += c;
    seen.push({ method: req.method, url: req.url, auth: req.headers.authorization, body });
    res.writeHead(418, { 'content-type': 'application/json' }).end('{"ok":1}');
  },
};
const broker = credentialBroker();
const granted = new Set(['demo']);
const gateway = appsGateway({ demo, other: demo }, broker.sessionOf, async (key) => (key === 'allowed' ? granted : new Set()));

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

test('a page with no credential passes, and never gets a 401 or 403', async () => {
  seen.length = 0;
  assert.equal((await fetch(`${base}/apps/other/x`)).status, 418);
  assert.equal(seen[0].auth, undefined);
});

test('an unmounted app is a 404 problem, and a non-app path is left alone', async () => {
  const r = await fetch(`${base}/apps/nope/x`, { headers: as(allowed) });
  assert.equal(r.status, 404);
  assert.equal(((await r.json()) as { detail: string }).detail, 'no such app');
  assert.equal((await fetch(`${base}/apps/constructor`)).status, 404);
  assert.equal(await (await fetch(`${base}/sessions`)).text(), 'other');
});

// fetch would resolve `..` before sending; a raw request sends the path as an attacker would.
const rawGet = (path: string) => new Promise<number>((resolve, reject) => request(base + '/', { path }, (r) => (r.resume(), resolve(r.statusCode!))).on('error', reject).end());

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
  const manifests: ExtensionManifest[] = ['a', 'b'].map((name) => ({ name, app: (ctx) => (built.push(`${name}:${ctx.dbPath}`), demo) }));
  const file: DeploymentFile = { extensions: { a: {} } };
  const apps = await mountApps(file, manifests, { distributorUrl: 'http://d', env: { QUACK_DATA_DIR: '/data' } });
  assert.deepEqual([Object.keys(apps), built], [['a'], ['a:/data/a.db']]);
  await assert.rejects(mountApps({ extensions: { c: {} } }, manifests, { distributorUrl: 'http://d', env: { QUACK_DATA_DIR: '/data' } }), /extensions\.c: no extension named c mounts an app/);
});
