import assert from 'node:assert/strict';
import { openCatalog } from '@quack/catalog';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import type { AddressInfo, Server } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxProvider } from '@earendil-works/pi-ai';
import { Artifacts } from '@quack/sdk';
import { openSession } from '@quack/session';
import { crossSession } from '@quack/runtime/catalog';
import { distributor } from '../../home/src/client.ts';
import { withCredential } from '../../home/src/manifest.ts';
import { readSession } from '../../home/src/tools.ts';
import { createDistributor, credentialBroker, processDriver, sessionDirName, type HostDriver } from '../src/index.ts';
import { pinnedAgentPath } from '../src/process-driver.ts';
import { PERSON, PROXY_SECRET } from './fixtures/proxy.ts';

const cleanups: Array<() => unknown> = [];
after(async () => {
  for (const fn of cleanups.reverse()) await fn();
});

const listening = async (server: Server) => {
  await new Promise((r) => server.listen(0, '127.0.0.1', () => r(undefined)));
  cleanups.push(() => new Promise((r) => server.close(r)));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
};

// Waits for a state change; the deadline only matters when the code is broken, so it is far above any runner's latency.
async function until<T>(probe: () => Promise<T | undefined>, ms = 15_000): Promise<T> {
  const deadline = Date.now() + ms;
  for (;;) {
    const v = await probe();
    if (v !== undefined) return v;
    if (Date.now() > deadline) throw new Error('timed out');
    await new Promise((r) => setTimeout(r, 20));
  }
}

// One distributor whose sessions run the gated fixture: `quack` selects the home extension, `reviewer` does not.
async function start() {
  const [dataDir, agentsDir] = [await mkdtemp(join(tmpdir(), 'quack-auth-')), await mkdtemp(join(tmpdir(), 'quack-auth-agents-'))];
  cleanups.push(() => rm(dataDir, { recursive: true, force: true }), () => rm(agentsDir, { recursive: true, force: true }));
  for (const [name, extensions] of [['quack', { home: {} }], ['reviewer', {}]] as const) {
    await mkdir(join(agentsDir, name));
    await writeFile(join(agentsDir, name, 'agent.json'), JSON.stringify({ name, version: 1, prompt: 'prompt.md', extensions }));
    await writeFile(join(agentsDir, name, 'prompt.md'), 'prompt');
  }
  const broker = credentialBroker();
  // The launch credential each session holds, as its own tool shell could read it.
  const held = new Map<string, string>();
  const mint = (key: string) => { const t = broker.mint(key); held.set(key, t); return t; };
  const driver = processDriver({ command: [process.execPath, fileURLToPath(new URL('./fixtures/gate-session.ts', import.meta.url))], dataDir, agentsDir, broker: { mint, revoke: broker.revoke } });
  cleanups.push(() => Promise.all(driver.running().map((k) => driver.stop(k))));
  const sessions = { sessionOf: broker.sessionOf, crossSession: async (key: string) => crossSession(await readFile(pinnedAgentPath(dataDir, key), 'utf8').then(JSON.parse, () => null)) };
  // The catalog the sessions' own channels fill; this fixture's sessions send none, so a test publishes what they would.
  const catalog = openCatalog({ dataDir: await mkdtemp(join(tmpdir(), 'quack-auth-catalog-')) });
  cleanups.push(() => catalog.close());
  const server = createDistributor(driver, { proxySecret: PROXY_SECRET, sessions, catalog });
  const [base, sessionBase] = [await listening(server), await listening(server.sessions)];
  const path = (key: string, rest = '') => `/sessions/${encodeURIComponent(key)}${rest}`;
  const invoke = async (key: string, agent: string) => assert.equal((await fetch(`${base}${path(key, '/invocations')}?agent=${agent}`, { method: 'POST', body: JSON.stringify({ input: 'post it' }) })).status, 202);
  const pending = (key: string) => until(async () => {
    const { approvals } = (await (await fetch(base + path(key, '/approvals'))).json()) as { approvals: Array<{ id: string }> };
    return approvals.length ? approvals : undefined;
  });
  const asSession = (key: string, url: string, init: RequestInit = {}) => fetch(sessionBase + url, { ...init, headers: { authorization: `Bearer ${held.get(key)}`, ...(init.headers as Record<string, string>) } });
  return { driver, base, sessionBase, path, invoke, pending, asSession, held, catalog, dataDir };
}

test('an identity header from a session is refused on both listeners, so it can delete or rename nothing', async () => {
  const { base, path, invoke, pending, asSession, catalog } = await start();
  const key = 'github:o/r#1';
  await invoke(key, 'reviewer');
  await pending(key);
  await catalog.publish(key, [{ seq: 1, id: `session:${key}`, at: 0, type: 'session.created', data: { agent: 'reviewer', createdAt: 0 } }]);
  const forged = await fetch(base + path(key), { method: 'DELETE', headers: { 'x-authentik-username': 'jason' } });
  assert.equal(forged.status, 401, 'the public listener believes an identity only beside the proxy\'s proof');
  // The Sessions API refuses a session credential's write (401); any other route is the listener's policy (403).
  for (const [method, rest, status] of [['DELETE', '', 401], ['PATCH', '', 401], ['POST', '/delete', 403]] as const) {
    const res = await asSession(key, path(key, rest), { method, headers: { ...PERSON, 'content-type': 'application/json' }, body: method === 'DELETE' ? undefined : '{"title":"x"}' });
    assert.equal(res.status, status, `${method} ${rest} from a session, even with the proxy's proof`);
  }
  assert.equal((await fetch(base + path(key))).status, 200, 'the session is still there');
});

test('a session cannot approve its own pending action through the distributor or its own server', async () => {
  const { driver, base, path, invoke, pending, asSession, held } = await start();
  const key = 'github:o/r#2';
  await invoke(key, 'reviewer');
  const [{ id }] = await pending(key);
  const approve = { method: 'POST', body: JSON.stringify({ decision: 'approve' }) };
  const viaDistributor = await asSession(key, path(key, `/approvals/${id}`), { ...approve, headers: { ...PERSON, 'content-type': 'application/json' } });
  assert.equal(viaDistributor.status, 403);
  const own = await driver.ensure(key);
  for (const headers of [{ 'x-authentik-username': 'jason' }, { authorization: `Bearer ${held.get(key)}`, 'x-quack-principal': encodeURIComponent('{"kind":"person","name":"jason"}') }] as Array<Record<string, string>>) {
    assert.equal((await fetch(`${own}/approvals/${id}`, { ...approve, headers })).status, 401, 'the session server takes only its launch token, which no shell holds');
  }
  assert.equal((await pending(key)).length, 1, 'still waiting on a person');
  assert.equal((await fetch(base + path(key, `/approvals/${id}`), { ...approve, headers: PERSON })).status, 200, 'the person approves through the proxy');
});

test('the home agent lists and reads other sessions; any other agent reaches only its own key', async () => {
  const { sessionBase, path, invoke, pending, asSession, held, catalog } = await start();
  await invoke('quack:home', 'quack');
  await invoke('github:o/r#3', 'reviewer');
  await Promise.all([pending('quack:home'), pending('github:o/r#3')]);
  for (const key of ['quack:home', 'github:o/r#3']) await catalog.publish(key, [{ seq: 1, id: `session:${key}`, at: 0, type: 'session.created', data: { agent: 'a', createdAt: 0 } }]);
  const home = distributor(sessionBase, withCredential(held.get('quack:home')!));
  assert.deepEqual((await home.sessions()).map((s) => s.key).sort(), ['github:o/r#3', 'quack:home']);
  assert.equal((await asSession('quack:home', path('github:o/r#3'))).status, 404, 'the home grant covers the lists, not another key');
  assert.equal((await home.session('github:o/r#3'))?.key, 'github:o/r#3', 'so the home client reads one session through them');
  assert.deepEqual([await home.artifacts('github:o/r#3'), await home.pendingAsks('github:o/r#3')], [[], []]);
  const reviewer = 'github:o/r#3';
  for (const url of ['/sessions', '/asks', '/artifacts', path('quack:home')]) assert.equal((await asSession(reviewer, url)).status, 404, url);
  for (const url of [path('quack:home', '/messages'), '/index/approvals']) assert.equal((await asSession(reviewer, url)).status, 403, url);
  assert.equal((await asSession(reviewer, path(reviewer, '/ping'))).status, 200, 'its own key');
  assert.equal((await fetch(sessionBase + '/sessions')).status, 401, 'no credential, no answer');
});

// Seals a version straight into a stopped session's store, as its artifacts extension would have.
async function sealInto(dir: string, kind: string, content: string) {
  const faux = fauxProvider({ models: [{ id: 'seal' }] });
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id } } });
  await session.root.commit(async (tx) => void ((await tx.doc(Artifacts, session.root.id)).kinds[kind] = { versions: [{ kind, version: 1, hash: 'h', content, sealedAt: 1, verdict: null }] }), ctx);
  await session.close();
}

test("the home agent reads a stopped session's messages and newest result in full through its export, without booting it", async () => {
  const { driver, invoke, pending, held, catalog, sessionBase, dataDir } = await start();
  const key = 'research:q1';
  const report = '## Summary\nAir scatters blue most.\n\n## Findings\n- Rayleigh scattering goes as 1/λ⁴. [s1]';
  await invoke('quack:home', 'quack');
  await invoke(key, 'reviewer');
  await Promise.all([pending('quack:home'), pending(key)]);
  await driver.stop(key);
  await sealInto(join(dataDir, sessionDirName(key)), 'research.report', report);
  await catalog.publish('quack:home', [{ seq: 1, id: 'session:quack:home', at: 0, type: 'session.created', data: { agent: 'quack', createdAt: 0 } }]);
  await catalog.publish(key, [
    { seq: 1, id: `session:${key}`, at: 0, type: 'session.created', data: { agent: 'reviewer', createdAt: 0 } },
    { seq: 2, id: `artifact:${key}:1`, at: 1, type: 'artifact.sealed', data: { kind: 'research.report', version: 1, hash: 'h', content: report, summary: 'Air scatters blue most.' } },
  ]);
  const home = distributor(sessionBase, withCredential(held.get('quack:home')!));
  const text = await readSession(home, (await home.session(key))!);
  assert.match(text, /- research\.report v1: Air scatters blue most\./);
  assert.ok(text.endsWith(`--- research.report v1 ---\n${report}`), text);
  assert.match(text, /Last messages:\n- user: post it\n/);
  assert.ok(!driver.running().includes(key), 'the read started no process');
});

test('the public listener drops a peer it is told to refuse, such as a session container', async () => {
  const driver = { running: () => [], wakes: async () => [], credential: () => undefined } as unknown as HostDriver;
  const base = await listening(createDistributor(driver, { refuse: (address) => address === '127.0.0.1' }));
  await assert.rejects(fetch(`${base}/sessions`));
});
