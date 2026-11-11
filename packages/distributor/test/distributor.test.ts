import assert from 'node:assert/strict';
import { createServer as createHttpServer, request as httpRequest } from 'node:http';
import { connect, type AddressInfo } from 'node:net';
import { cp, mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { Agent, fetch as ufetch, getGlobalDispatcher, setGlobalDispatcher } from 'undici';
import { nextFrame, runEnded } from '../../session/test/ws-helpers.ts';
import { childPort, createdAt, shellEnv } from '@quack/session';
import { spawn } from 'node:child_process';
import { PassThrough, Writable } from 'node:stream';
import { RECORDS } from '../src/orphans.ts';
import { openCatalog } from '@quack/catalog';
import { createDistributor, forwardOutput, processDriver, sessionDirName, type HostDriver } from '../src/index.ts';
import { PING_TIMEOUT_MS, PROXY_PROOF } from '../src/server.ts';
import { PERSON, PROXY_SECRET } from './fixtures/proxy.ts';

const WAKE_RETRY_MAX = 5 * 60_000;

const dirs: string[] = [];
const servers: Array<() => Promise<void>> = [];
after(async () => {
  await Promise.all(servers.map((close) => close()));
  await Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true })));
});

// Each test gets its own distributor so idle reaping in one cannot stop another's session.
async function agentsDir(...names: string[]) {
  return agentsDirAt(3, ...names);
}

async function agentsDirAt(version: number, ...names: string[]) {
  const dir = await mkdtemp(join(tmpdir(), 'quack-agents-'));
  dirs.push(dir);
  for (const name of names) {
    await mkdir(join(dir, name));
    await writeFile(join(dir, name, 'agent.json'), JSON.stringify({ name, version, prompt: 'prompt.md' }));
    await writeFile(join(dir, name, 'prompt.md'), `prompt v${version}`);
  }
  return dir;
}

async function startDistributor(idleMs: number, agents: { agentsDir?: string; defaultAgent?: string; inherit?: () => NodeJS.ProcessEnv } = { defaultAgent: '' }, withCatalog = false) {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-dist-'));
  dirs.push(dataDir);
  const driver = processDriver({ command: [process.execPath, fileURLToPath(new URL('./fixtures/echo-session.ts', import.meta.url))], dataDir, ...agents });
  // A fake clock with no timers: a test runs the idle sweep itself after moving the clock.
  let clock = 1_000_000;
  // The catalog lives beside, not in, the data dir, so a test can still see that dir emptied.
  const catalog = withCatalog ? openCatalog({ dataDir: await mkdtemp(join(tmpdir(), 'quack-dist-catalog-')) }) : undefined;
  if (catalog) servers.push(() => catalog.close());
  const server = createDistributor(driver, { idleMs, catalog, proxySecret: PROXY_SECRET, now: () => clock, setTimer: () => () => {} }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  servers.push(async () => {
    await Promise.all(driver.running().map((k) => driver.stop(k)));
    await new Promise((r) => server.close(r));
  });
  const invoke = (key: string, body: unknown, agent?: string) =>
    fetch(`${base}/sessions/${encodeURIComponent(key)}/invocations${agent ? `?agent=${agent}` : ''}`, { method: 'POST', body: JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json() }));
  const ranAs = (key: string) => `agent: ${join(dataDir, sessionDirName(key), 'agent-def', 'agent.json')}`;
  // What a session's own lifecycle channel would have reported, for the tests that delete through the Sessions API.
  const known = (key: string) => catalog!.publish(key, [{ seq: 1, id: `session:${key}`, at: clock, type: 'session.created', data: { agent: 'quack', createdAt: clock } }]);
  const exported = async (key: string) => ((await (await fetch(`${base}/sessions/${encodeURIComponent(key)}/export`)).json()) as { session: { agent: string; version?: number } }).session;
  return { driver, invoke, base, ranAs, dataDir, known, exported, sweep: () => server.sweep(), advance: (ms: number) => void (clock += ms) };
}

// Waits for a state change; the deadline only matters when the code is broken, so it is far above any runner's latency.
async function until(probe: () => boolean, ms = 15_000) {
  const deadline = Date.now() + ms;
  while (!probe()) {
    if (Date.now() > deadline) throw new Error('timed out');
    await new Promise((r) => setTimeout(r, 20));
  }
}

test('routes each session key to its own process', async () => {
  const { driver, invoke } = await startDistributor(60_000);
  const pr = await invoke('github:fagerbergj/quack#1826', { input: 'review it', requestId: 'a', wait: true });
  assert.equal(pr.status, 200);
  assert.equal(pr.body.answer, 'echo: review it');
  const research = await invoke('research:spec-decoding', { input: 'look it up', wait: true });
  assert.equal(research.body.answer, 'echo: look it up');
  assert.deepEqual(driver.running().sort(), ['github:fagerbergj/quack#1826', 'research:spec-decoding']);
  assert.notEqual(driver.pid('github:fagerbergj/quack#1826'), driver.pid('research:spec-decoding'));

  const bad = await invoke('no spaces allowed', { input: 'x' });
  assert.equal(bad.status, 400);
});

test('a killed session process restarts with its history', async () => {
  const { driver, invoke } = await startDistributor(60_000);
  const key = 'github:fagerbergj/quack#1826';
  const first = await invoke(key, { input: 'review it', requestId: 'a', wait: true });
  const before = driver.pid(key)!;
  process.kill(before, 'SIGKILL');
  await until(() => driver.pid(key) !== undefined && driver.pid(key) !== before);
  const retried = await invoke(key, { input: 'review it', requestId: 'a', wait: true });
  assert.equal(retried.body.answer, 'echo: review it');
  assert.equal(retried.body.submissionId, first.body.submissionId, 'the retry found the first run in the restored log');
});

test('idle sessions release their process and wake on the next request', async () => {
  const { driver, invoke, sweep, advance } = await startDistributor(60_000);
  await invoke('research:spec-decoding', { input: 'first', wait: true });
  advance(59_999);
  await sweep();
  assert.deepEqual(driver.running(), ['research:spec-decoding'], 'not idle yet');
  advance(1);
  await sweep();
  assert.deepEqual(driver.running(), []);
  const woke = await invoke('research:spec-decoding', { input: 'again', wait: true });
  assert.equal(woke.body.answer, 'echo: again');
});

test('a session that keeps crashing stops auto-restarting until the next request', async () => {
  const { driver, invoke } = await startDistributor(60_000);
  const key = 'research:crashy';
  await invoke(key, { input: 'first', wait: true });
  for (let i = 0; i < 4; i++) {
    const pid = driver.pid(key);
    if (pid === undefined) break;
    process.kill(pid, 'SIGKILL');
    await until(() => driver.pid(key) !== pid);
    if (i < 3) await until(() => driver.pid(key) !== undefined);
  }
  assert.equal(driver.pid(key), undefined, 'the fourth crash in a minute is not restarted');
  const woke = await invoke(key, { input: 'again', wait: true });
  assert.equal(woke.body.answer, 'echo: again');
});

test('a malformed session key is a client error', async () => {
  const { base } = await startDistributor(60_000);
  assert.equal((await fetch(`${base}/sessions/%ZZ/invocations`, { method: 'POST', body: '{}' })).status, 400);
});

const PROBLEM = 'application/problem+json';

test('the distributor answers its own errors as problem+json: 404 for an unknown key, 400 for a bad one', async () => {
  const { base } = await startDistributor(60_000);
  const missing = await fetch(`${base}/sessions/research%3Aghost/messages`);
  assert.equal(missing.status, 404);
  assert.equal(missing.headers.get('content-type'), PROBLEM);
  assert.deepEqual(await missing.json(), { type: 'about:blank', title: 'Not Found', status: 404, detail: 'no such session', error: 'no such session' });
  const bad = await fetch(`${base}/sessions/%ZZ/invocations`, { method: 'POST', body: '{}' });
  assert.equal(bad.headers.get('content-type'), PROBLEM);
  assert.deepEqual(await bad.json(), { type: 'about:blank', title: 'Bad Request', status: 400, detail: 'invalid session key', error: 'invalid session key' });
});

test('an unknown or deleted key answers 404 on every route and starts nothing; an invocation still creates it', async () => {
  const { driver, base, invoke, dataDir } = await startDistributor(60_000);
  const key = encodeURIComponent('research:ghost');
  const routes: Array<[string, string]> = [['GET', 'messages'], ['HEAD', 'artifacts'], ['GET', 'ping'], ['GET', 'approvals'], ['POST', 'questions/q1'], ['POST', 'approvals/a1'], ['POST', 'abort'], ['POST', 'resume']];
  for (const [method, path] of routes) assert.equal((await fetch(`${base}/sessions/${key}/${path}`, { method, ...(method === 'POST' && { body: '{}' }) })).status, 404, `${method} ${path}`);
  const ws = new WebSocket(`${base.replace('http', 'ws')}/sessions/${key}/ws`);
  await new Promise((r) => { ws.onerror = ws.onclose = r; });
  assert.deepEqual(driver.running(), []);
  assert.deepEqual((await readdir(dataDir)).filter((d) => d !== RECORDS), []);
  assert.equal((await invoke('research:ghost', { input: 'hi', wait: true })).status, 200);
  assert.equal((await fetch(`${base}/sessions/${key}/ping`)).status, 200);
});

test('a session open in a page is deleted from that page: its idle process is stopped and its socket ends', async () => {
  const { driver, base, invoke, dataDir, known } = await startDistributor(60_000, { defaultAgent: '' }, true);
  const key = 'github:fagerbergj/quack#2208';
  await invoke(key, { input: 'start', wait: true });
  await known(key);
  const ws = new WebSocket(`${base.replace('http', 'ws')}/sessions/${encodeURIComponent(key)}/ws`);
  assert.equal((await nextFrame(ws, () => true)).type, 'view');
  const closed = new Promise((r) => { ws.onclose = r; });
  try {
    const res = await fetch(`${base}/sessions/${encodeURIComponent(key)}`, { method: 'DELETE', headers: PERSON });
    assert.equal(res.status, 200);
    await closed;
    assert.deepEqual(driver.running(), []);
    assert.deepEqual((await readdir(dataDir)).filter((d) => d !== RECORDS), []);
  } finally {
    // A failed delete must not leave the socket holding the session open past the test.
    ws.close();
  }
});

test('proxies the live stream and keeps a watched session from idling out', async () => {
  const { driver, base, invoke, sweep, advance } = await startDistributor(60_000);
  const key = 'github:fagerbergj/quack#1827';
  await invoke(key, { input: 'start', wait: true });
  const ws = new WebSocket(`${base.replace('http', 'ws')}/sessions/${encodeURIComponent(key)}/ws`);
  assert.equal((await nextFrame(ws, () => true)).type, 'view');

  const ended = nextFrame(ws, runEnded());
  ws.send(JSON.stringify({ type: 'input', input: 'hi', requestId: 'w-1' }));
  assert.match(JSON.stringify((await ended).items), /echo: hi/);

  // Idle sweeps long past the idle time while the viewer is connected; the session must still be running.
  for (let i = 0; i < 3; i++) {
    advance(60_000);
    await sweep();
  }
  assert.deepEqual(driver.running(), [key]);
  ws.close();
  // The proxy's side of the socket closes a few I/O turns after the page's; sweeps on the fake clock then stop the session.
  for (let i = 0; i < 1_000 && driver.running().length; i++) {
    await new Promise((r) => setImmediate(r));
    advance(60_000);
    await sweep();
  }
  assert.deepEqual(driver.running(), []);
});

test('a viewer whose session cannot start is disconnected, and the distributor keeps serving', async () => {
  const broken = { agents: async () => [], agentOf: async () => undefined, rebind: async () => {}, agent: async () => undefined, promote: async () => 'draft' as const, pinned: async () => ({}), exported: async () => ({ transcript: [], artifacts: [] }), ensure: async () => { throw new Error('no capacity'); }, stop: async () => {}, remove: async () => false, setWake: async () => {}, wakes: async () => [], running: () => [], pid: () => undefined, credential: () => undefined };
  const server = createDistributor(broken, { idleMs: 60_000 }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const ws = new WebSocket(`${base.replace('http', 'ws')}/sessions/research%3Ax/ws`);
  const closed = await new Promise<string>((r) => { ws.onerror = () => r('error'); ws.onopen = () => r('open'); });
  assert.equal(closed, 'error');
  assert.equal((await fetch(`${base}/agents`)).status, 200);
  await new Promise((r) => server.close(r));
});

test('each session runs on the agent that created it, and the registry lists the agents', async () => {
  const dir = await agentsDir('reviewer', 'research');
  const { invoke, base, driver, ranAs, known, exported } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' }, true);
  const whoami = (key: string, agent?: string) => invoke(key, { input: 'whoami', wait: true }, agent);
  assert.equal((await whoami('github:a#1', 'reviewer')).body.answer, ranAs('github:a#1'));
  assert.equal((await whoami('research:b', 'research')).body.answer, ranAs('research:b'));

  const { agents } = (await (await fetch(`${base}/agents`)).json()) as { agents: Array<{ name: string }> };
  assert.deepEqual(agents.sort((a, b) => a.name.localeCompare(b.name)), [{ name: 'research', version: 3, status: 'draft' }, { name: 'reviewer', version: 3, status: 'draft' }]);
  await driver.stop('research:b');
  await Promise.all(['github:a#1', 'research:b'].map(known));
  // The export names the agent the session is bound to, not the one the catalog's first event carried.
  assert.deepEqual([(await exported('github:a#1')).agent, (await exported('research:b')).agent], ['reviewer', 'research']);
});

test('a new session records its creation time once, where the session facts read it', async () => {
  const { invoke, dataDir } = await startDistributor(60_000);
  const start = Date.now();
  await invoke('ord:1', { input: 'hi', wait: true });
  const at = await createdAt(join(dataDir, sessionDirName('ord:1')));
  assert.ok(at >= start && at <= Date.now());
});

test('a session keeps its agent on requests that are not submissions; an unknown one is 404', async () => {
  const dir = await agentsDir('reviewer', 'research');
  const { invoke, ranAs, base } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  await invoke('research:x', { input: 'hi', wait: true }, 'research');
  assert.equal((await fetch(`${base}/sessions/research%3Ax/ping?agent=reviewer`)).status, 409);
  assert.equal((await invoke('research:x', { input: 'hi', wait: true }, 'research')).status, 200);
  assert.equal((await invoke('research:x', { input: 'whoami', wait: true })).body.answer, ranAs('research:x'));
  assert.equal((await invoke('research:y', { input: 'hi' }, 'nope')).status, 404);
});

test('a session naming no agent gets the deployment default', async () => {
  const dir = await agentsDir('reviewer', 'research');
  const defaultAgent = join(dir, 'reviewer', 'agent.json');
  const { invoke, base, ranAs, driver } = await startDistributor(60_000, { agentsDir: dir, defaultAgent });
  assert.equal((await invoke('github:a#2', { input: 'whoami', wait: true })).body.answer, ranAs('github:a#2'));
  assert.equal((await invoke('github:a#2', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  assert.equal((await fetch(`${base}/sessions/github%3Aa%232/ping?agent=research`)).status, 409);
  assert.equal(await driver.agentOf('github:a#2'), 'reviewer');
});

test('a registry agent sharing the default agent\'s name runs the registry file', async () => {
  const dir = await agentsDir('reviewer');
  const other = await agentsDir('reviewer');
  const { invoke, ranAs } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: join(other, 'reviewer', 'agent.json') });
  assert.equal((await invoke('github:a#3', { input: 'whoami', wait: true }, 'reviewer')).body.answer, ranAs('github:a#3'));
});

test('a session stays on the version it started on after the agents dir moves on; a new one gets the newest', async () => {
  const dir = await agentsDirAt(1, 'reviewer');
  const { invoke, driver, ranAs, known, exported } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' }, true);
  await invoke('github:v#1', { input: 'hi', wait: true }, 'reviewer');
  await driver.stop('github:v#1');
  await agentsDirAt(2, 'reviewer').then((next) => cp(join(next, 'reviewer'), join(dir, 'reviewer'), { recursive: true }));

  assert.equal((await invoke('github:v#1', { input: 'whoami', wait: true })).body.answer, ranAs('github:v#1'));
  assert.equal((await invoke('github:v#2', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  const pinned = async (key: string) => JSON.parse(await readFile(ranAs(key).slice('agent: '.length), 'utf8')).version;
  assert.deepEqual([await pinned('github:v#1'), await pinned('github:v#2')], [1, 2]);
  const prompt = (key: string) => readFile(join(ranAs(key).slice('agent: '.length), '..', 'prompt.md'), 'utf8');
  assert.deepEqual([await prompt('github:v#1'), await prompt('github:v#2')], ['prompt v1', 'prompt v2']);
  await Promise.all(['github:v#1', 'github:v#2'].map(known));
  assert.deepEqual([(await exported('github:v#1')).version, (await exported('github:v#2')).version], [1, 2]);
});

test('an agent version whose content changed is refused for new sessions but not existing ones', async () => {
  const dir = await agentsDirAt(1, 'reviewer');
  const { invoke } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  assert.equal((await invoke('github:f#1', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  await writeFile(join(dir, 'reviewer', 'prompt.md'), 'edited without a version bump');
  assert.equal((await invoke('github:f#2', { input: 'hi', wait: true }, 'reviewer')).status, 409);
  assert.equal((await invoke('github:f#1', { input: 'hi', wait: true })).status, 200);
});

test('a bundle that dropped a key is refused for new sessions until its version is bumped', async () => {
  const dir = await agentsDirAt(1, 'reviewer');
  const file = join(dir, 'reviewer', 'agent.json');
  const old = { name: 'reviewer', version: 1, status: 'promoted', model: 'local/qwen3.8-27b', maxTurns: 80, prompt: 'prompt.md', extensions: {} };
  await writeFile(file, JSON.stringify(old));
  const { invoke, ranAs } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  assert.equal((await invoke('github:m#1', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  const { maxTurns: _, ...cut } = old;
  await writeFile(file, JSON.stringify(cut));
  assert.equal((await invoke('github:m#2', { input: 'hi', wait: true }, 'reviewer')).status, 409);
  await writeFile(file, JSON.stringify({ ...cut, version: 2 }));
  assert.equal((await invoke('github:m#2', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  const pinned = async (key: string) => JSON.parse(await readFile(ranAs(key).slice('agent: '.length), 'utf8'));
  assert.equal((await pinned('github:m#1')).maxTurns, 80);
  assert.equal((await pinned('github:m#2')).version, 2);
});

test('QUACK_WEB_DIR serves files, refuses traversal, and leaves API routes alone', async () => {
  const web = await mkdtemp(join(tmpdir(), 'quack-web-'));
  dirs.push(web);
  await mkdir(join(web, 'assets'));
  await writeFile(join(web, 'index.html'), '<h1>quack</h1>');
  await writeFile(join(web, 'assets', 'app.js'), 'x');
  await writeFile(join(web, 'manifest.webmanifest'), '{}');
  const driver = processDriver({ command: [process.execPath, fileURLToPath(new URL('./fixtures/echo-session.ts', import.meta.url))], dataDir: web, defaultAgent: '' });
  const server = createDistributor(driver, { webDir: web }).listen(0);
  await new Promise((r) => server.once('listening', r));
  servers.push(() => new Promise((r) => server.close(() => r())));
  const port = (server.address() as AddressInfo).port;
  const base = `http://127.0.0.1:${port}`;
  const index = await fetch(`${base}/`);
  assert.equal(index.status, 200);
  assert.match(index.headers.get('content-type')!, /^text\/html/);
  assert.equal(await index.text(), '<h1>quack</h1>');
  assert.match((await fetch(`${base}/assets/app.js`)).headers.get('content-type')!, /^text\/javascript/);
  assert.equal((await fetch(`${base}/manifest.webmanifest`)).headers.get('content-type'), 'application/manifest+json');
  assert.equal(index.headers.get('cache-control'), 'no-cache');
  assert.equal((await fetch(`${base}/manifest.webmanifest`)).headers.get('cache-control'), 'no-cache');
  const asset = await fetch(`${base}/assets/app.js`);
  assert.equal(asset.headers.get('cache-control'), 'public, max-age=31536000, immutable');
  const etag = index.headers.get('etag')!;
  assert.ok(etag);
  assert.equal((await fetch(`${base}/`, { headers: { 'if-none-match': etag } })).status, 304);
  assert.equal((await fetch(`${base}/`, { headers: { 'if-none-match': '"stale"' } })).status, 200);
  assert.equal((await fetch(`${base}/assets/`)).status, 404);
  assert.equal((await fetch(`${base}/missing.js`)).status, 404);
  // fetch normalizes dot segments, so the traversal goes out as a raw request line.
  const raw = await new Promise<string>((res) => {
    const s = connect(port, '127.0.0.1', () => s.write('GET /%2e%2e/%2e%2e/etc/passwd HTTP/1.1\r\nhost: x\r\nconnection: close\r\n\r\n'));
    let out = '';
    s.on('data', (d) => (out += d)).on('close', () => res(out));
  });
  assert.match(raw, /^HTTP\/1.1 404/);
  assert.deepEqual(await (await fetch(`${base}/agents`)).json(), { agents: [] });
});

test('without QUACK_WEB_DIR nothing is served', async () => {
  const { base } = await startDistributor(60_000);
  assert.equal((await fetch(`${base}/`)).status, 404);
  assert.equal((await fetch(`${base}/index.html`)).status, 404);
});

test('promoting a version does not fork it, but any other edit without a bump does', async () => {
  const dir = await agentsDirAt(1, 'reviewer');
  const { invoke, base } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  assert.equal((await invoke('github:p#1', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  const file = join(dir, 'reviewer', 'agent.json');
  const def = JSON.parse(await readFile(file, 'utf8'));
  await writeFile(file, JSON.stringify({ ...def, status: 'promoted' }));
  assert.equal((await invoke('github:p#2', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  const { agents } = (await (await fetch(`${base}/agents`)).json()) as { agents: Array<{ status: string }> };
  assert.equal(agents[0].status, 'promoted');
  await writeFile(file, JSON.stringify({ ...def, status: 'promoted', note: 'edited' }));
  assert.equal((await invoke('github:p#3', { input: 'hi', wait: true }, 'reviewer')).status, 409);
});

test('GET /version reports the build from the environment, null when unset', async () => {
  const { base } = await startDistributor(60_000);
  const saved = [process.env.QUACK_COMMIT, process.env.QUACK_BUILT_AT];
  try {
    delete process.env.QUACK_COMMIT;
    delete process.env.QUACK_BUILT_AT;
    assert.deepEqual(await (await fetch(`${base}/version`)).json(), { commit: null, builtAt: null });
    process.env.QUACK_COMMIT = 'abc1234def';
    process.env.QUACK_BUILT_AT = '2026-11-03T12:00:00Z';
    assert.deepEqual(await (await fetch(`${base}/version`)).json(), { commit: 'abc1234def', builtAt: '2026-11-03T12:00:00Z' });
  } finally {
    for (const [k, v] of [['QUACK_COMMIT', saved[0]], ['QUACK_BUILT_AT', saved[1]]] as const) {
      if (v === undefined) delete process.env[k]; else process.env[k] = v;
    }
  }
});

test('GET /agents reports a promoted agent as promoted', async () => {
  const dir = await agentsDirAt(3, 'reviewer');
  await writeFile(join(dir, 'reviewer', 'agent.json'), JSON.stringify({ name: 'reviewer', version: 3, status: 'promoted', prompt: 'prompt.md' }));
  const { base } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  const { agents } = (await (await fetch(`${base}/agents`)).json()) as { agents: unknown[] };
  assert.deepEqual(agents, [{ name: 'reviewer', version: 3, status: 'promoted' }]);
});

// A driver with no processes: sessions are stub /ping servers, so sleep and wake run on a fake clock.
async function fakeFleet(ping: (now: number) => unknown) {
  let clock = 1_000_000;
  // A ping answer of undefined is a session that never answers.
  const stub = createHttpServer((_req, res) => { const answer = ping(clock); if (answer !== undefined) res.end(JSON.stringify(answer)); }).listen(0);
  await new Promise((r) => stub.once('listening', r));
  servers.push(() => new Promise((r) => { stub.close(() => r()); stub.closeAllConnections(); }));
  const url = `http://127.0.0.1:${(stub.address() as AddressInfo).port}`;
  const wakes = new Map<string, number>();
  const running = new Set<string>();
  const log: string[] = [];
  const driver = {
    agents: async () => [], agentOf: async () => '', rebind: async () => {}, agent: async () => undefined, promote: async () => 'draft' as const, pid: () => undefined, credential: () => undefined,
    pinned: async () => ({}),
    ensure: async (key: string) => { if (!running.has(key)) log.push(`start ${key}`); running.add(key); return url; },
    stop: async (key: string) => { running.delete(key); log.push(`stop ${key}`); },
    remove: async () => false, exported: async () => ({ transcript: [], artifacts: [] }),
    setWake: async (key: string, at: number | undefined) => { if (at === undefined) wakes.delete(key); else wakes.set(key, at); },
    wakes: async () => [...wakes].map(([key, at]) => ({ key, at })),
    running: () => [...running],
  };
  const timers: Array<{ at: number; fn: () => void; live: boolean }> = [];
  const setTimer = (fn: () => void, ms: number) => {
    const t = { at: clock + ms, fn, live: true };
    timers.push(t);
    return () => { t.live = false; };
  };
  const advance = (ms: number) => {
    const end = clock + ms;
    for (;;) {
      const next = timers.filter((t) => t.live && t.at <= end).sort((a, b) => a.at - b.at)[0];
      if (!next) break;
      next.live = false;
      clock = next.at;
      next.fn();
    }
    clock = end;
  };
  const start = () => {
    const server = createDistributor(driver, { idleMs: 600_000, now: () => clock, setTimer });
    servers.push(() => new Promise((r) => server.close(() => r())));
    return server;
  };
  return { driver, wakes, log, advance, start, now: () => clock };
}

const HOUR = 3_600_000;

test('a session sleeping an hour is stopped by the sweep and restarted at its wake time', async () => {
  const wakeAt = 1_000_000 + HOUR;
  const fleet = await fakeFleet(() => ({ status: 'Healthy', waitingOnHuman: false, wake: { at: wakeAt } }));
  const server = fleet.start().listen(0);
  await new Promise((r) => server.once('listening', r));
  await fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/research%3Anap/ping`);
  fleet.log.length = 0;

  fleet.advance(300_000);
  await server.sweep();
  assert.deepEqual(fleet.log, [], 'not idle yet');

  fleet.advance(300_000);
  await server.sweep();
  assert.deepEqual(fleet.log, ['stop research:nap']);
  assert.equal(fleet.wakes.get('research:nap'), wakeAt, 'the wake time is recorded for a restarted distributor');

  fleet.advance(wakeAt - fleet.now() - 1);
  await server.sweep();
  assert.deepEqual(fleet.log, ['stop research:nap'], 'not before the wake time');
  fleet.advance(1);
  await server.sweep();
  assert.deepEqual(fleet.log, ['stop research:nap', 'start research:nap']);
  assert.equal(fleet.wakes.has('research:nap'), false, 'the record is cleared once woken');
});

test('reading approvals neither keeps a running session warm nor starts a stopped one', async () => {
  const fleet = await fakeFleet(() => ({ status: 'Healthy', waitingOnHuman: false, wake: {} }));
  const server = fleet.start().listen(0);
  await new Promise((r) => server.once('listening', r));
  const sessions = `http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/research%3Anap`;
  await fetch(`${sessions}/ping`);
  fleet.advance(500_000);
  assert.equal((await fetch(`${sessions}/approvals`)).status, 200);
  fleet.advance(150_000);
  await server.sweep();
  assert.deepEqual(fleet.log, ['start research:nap', 'stop research:nap'], 'the poll did not reset the idle clock');
  const idle = await (await fetch(`${sessions}/approvals`)).json();
  assert.deepEqual(idle, { approvals: [] });
  assert.deepEqual(fleet.log, ['start research:nap', 'stop research:nap'], 'a stopped session stays stopped');
});

test('a restarted distributor rebuilds wake timers from the recorded times', async () => {
  const fleet = await fakeFleet(() => ({ status: 'Healthy', waitingOnHuman: false, wake: {} }));
  fleet.wakes.set('research:nap', fleet.now() + HOUR);
  const server = fleet.start();
  await server.sweep();
  fleet.advance(HOUR);
  await server.sweep();
  assert.deepEqual(fleet.log, ['start research:nap']);
});

test('a drain records active sessions as due now, so the next distributor resumes them without a request', async () => {
  const fleet = await fakeFleet(() => ({ status: 'HealthyBusy', waitingOnHuman: false, wake: {} }));
  const old = fleet.start();
  await fleet.driver.ensure('github:a#7');
  await old.drain();
  assert.deepEqual(fleet.driver.running(), []);
  assert.equal(fleet.wakes.get('github:a#7'), fleet.now());
  fleet.advance(10 * HOUR);
  await old.sweep();
  assert.deepEqual(fleet.driver.running(), [], 'a draining distributor does not restart what it just stopped');

  const next = fleet.start();
  await next.sweep();
  fleet.advance(0);
  await next.sweep();
  assert.deepEqual(fleet.driver.running(), ['github:a#7']);
});

test('a crash after a sweep saw a session busy still restarts it on the next distributor', async () => {
  const fleet = await fakeFleet(() => ({ status: 'HealthyBusy', waitingOnHuman: false, wake: {} }));
  const old = fleet.start();
  await fleet.driver.ensure('sleeper:trends');
  await old.sweep();
  assert.equal(fleet.wakes.get('sleeper:trends'), fleet.now(), 'recorded by the sweep, before any shutdown');
  await fleet.driver.stop('sleeper:trends');

  const next = fleet.start();
  await next.sweep();
  fleet.advance(0);
  await next.sweep();
  assert.deepEqual(fleet.driver.running(), ['sleeper:trends']);
});

test('a session that went idle after a busy sweep is not restarted after a crash', async () => {
  let status = 'HealthyBusy';
  const fleet = await fakeFleet(() => ({ status, waitingOnHuman: false, wake: {} }));
  const old = fleet.start();
  await fleet.driver.ensure('sleeper:trends');
  await old.sweep();
  status = 'Healthy';
  await old.sweep();
  assert.equal(fleet.wakes.has('sleeper:trends'), false);
  await fleet.driver.stop('sleeper:trends');
  const next = fleet.start();
  await next.sweep();
  assert.deepEqual(fleet.driver.running(), []);
});

test('a drain whose ping fails keeps the busy marker from the last sweep', async () => {
  let fail = false;
  const fleet = await fakeFleet(() => ({ status: 'HealthyBusy', waitingOnHuman: false, wake: {} }));
  const server = fleet.start();
  await fleet.driver.ensure('sleeper:trends');
  await server.sweep();
  const ensure = fleet.driver.ensure;
  fleet.driver.ensure = async (key: string) => { if (fail) throw new Error('down'); return ensure(key); };
  fail = true;
  await server.drain();
  assert.equal(fleet.wakes.get('sleeper:trends'), fleet.now());
});

test('a busy session with a pending sleep gets that sleep back when it goes idle', async () => {
  const sleepAt = 1_000_000 + HOUR;
  let busy = true;
  const fleet = await fakeFleet(() => ({ status: busy ? 'HealthyBusy' : 'Healthy', waitingOnHuman: false, wake: { at: sleepAt } }));
  const server = fleet.start().listen(0);
  await new Promise((r) => server.once('listening', r));
  // A request sets the use clock, so the idle sweep keeps the session running and only the revert can write the record.
  await fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/sleeper%3Atrends/ping`);
  await server.sweep();
  assert.equal(fleet.wakes.get('sleeper:trends'), fleet.now());
  busy = false;
  await server.sweep();
  assert.equal(fleet.wakes.get('sleeper:trends'), sleepAt);
  assert.deepEqual(fleet.driver.running(), ['sleeper:trends']);
});

test('a marker write that fails is retried by the next sweep', async () => {
  const fleet = await fakeFleet(() => ({ status: 'HealthyBusy', waitingOnHuman: false, wake: {} }));
  const set = fleet.driver.setWake;
  let failed = false;
  fleet.driver.setWake = async (key: string, at: number | undefined) => { if (!failed) { failed = true; throw new Error('ENOSPC'); } return set(key, at); };
  const server = fleet.start();
  await fleet.driver.ensure('sleeper:trends');
  await server.sweep();
  assert.equal(fleet.wakes.has('sleeper:trends'), false);
  await server.sweep();
  assert.equal(fleet.wakes.get('sleeper:trends'), fleet.now());
});

test('a wake that fails is retried with backoff until it succeeds, and the failure is logged once', async (t) => {
  const warn = t.mock.method(console, 'warn', () => {});
  const fleet = await fakeFleet(() => ({ status: 'Healthy', waitingOnHuman: false, wake: {} }));
  fleet.wakes.set('research:nap', fleet.now() + HOUR);
  const ensure = fleet.driver.ensure;
  let failures = 2;
  fleet.driver.ensure = async (key: string) => { if (failures-- > 0) throw new Error('no capacity'); return ensure(key); };
  const server = fleet.start();
  await server.sweep();
  fleet.advance(HOUR);
  await server.sweep();
  assert.deepEqual([fleet.log, fleet.wakes.has('research:nap')], [[], true], 'the first wake failed and its record stays');
  fleet.advance(1000);
  await server.sweep();
  assert.deepEqual(fleet.log, [], 'the retry after 1 s failed too');
  fleet.advance(1999);
  await server.sweep();
  assert.deepEqual(fleet.log, [], 'the next retry waits 2 s');
  fleet.advance(1);
  await server.sweep();
  assert.deepEqual(fleet.log, ['start research:nap']);
  assert.equal(fleet.wakes.has('research:nap'), false);
  assert.equal(warn.mock.callCount(), 1);
});

test('a wake for a session deleted meanwhile is dropped, not retried', async () => {
  const fleet = await fakeFleet(() => ({ status: 'Healthy', waitingOnHuman: false, wake: {} }));
  fleet.wakes.set('research:gone', fleet.now() + HOUR);
  fleet.driver.agentOf = async () => undefined as unknown as string;
  const server = fleet.start();
  await server.sweep();
  fleet.advance(HOUR + 10 * WAKE_RETRY_MAX);
  await server.sweep();
  assert.deepEqual(fleet.log, []);
});

test('a busy session whose process is gone without a restart is woken by the sweep', async (t) => {
  t.mock.method(console, 'warn', () => {});
  const fleet = await fakeFleet(() => ({ status: 'HealthyBusy', waitingOnHuman: false, wake: {} }));
  const server = fleet.start();
  await fleet.driver.ensure('sleeper:trends');
  await server.sweep();
  // The process is gone and the driver gave up restarting it: past its crash budget, or its restart failed.
  await fleet.driver.stop('sleeper:trends');
  fleet.log.length = 0;
  await server.sweep();
  fleet.advance(1000);
  await server.sweep();
  assert.deepEqual(fleet.log, ['start sleeper:trends']);
  assert.equal(fleet.wakes.get('sleeper:trends'), fleet.now() - 1000, 'the busy marker stays on disk while it runs');
});

test('a session that never answers /ping is left at the deadline, and the sweep goes on', async () => {
  let answer = true;
  const fleet = await fakeFleet((now) => (answer ? { status: 'Healthy', waitingOnHuman: false, wake: {}, now } : undefined));
  const server = fleet.start();
  await fleet.driver.ensure('research:stuck');
  answer = false;
  let done = false;
  const swept = server.sweep().then(() => (done = true));
  await new Promise((r) => setImmediate(r));
  assert.equal(done, false);
  fleet.advance(PING_TIMEOUT_MS);
  await swept;
  assert.deepEqual(fleet.driver.running(), ['research:stuck'], 'left for the next sweep');
});

test('active work is never stopped', async () => {
  const fleet = await fakeFleet(() => ({ status: 'HealthyBusy', waitingOnHuman: false, wake: {} }));
  const server = fleet.start();
  await fleet.driver.ensure('research:busy');
  fleet.advance(2 * HOUR);
  await server.sweep();
  assert.deepEqual(fleet.driver.running(), ['research:busy']);
});

test('a session waiting on a human verdict is stopped, and the verdict restarts it so the gated tool runs', async () => {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-dist-'));
  dirs.push(dataDir);
  const key = 'github:a#9';
  const driver = processDriver({ command: [process.execPath, fileURLToPath(new URL('./fixtures/gate-session.ts', import.meta.url))], dataDir, defaultAgent: '' });
  let clock = 1_000_000;
  const server = createDistributor(driver, { idleMs: 60_000, now: () => clock, setTimer: () => () => {} }).listen(0);
  await new Promise((r) => server.once('listening', r));
  servers.push(async () => {
    await server.drain();
    await new Promise((r) => server.close(r));
  });
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/${encodeURIComponent(key)}`;
  const get = (path: string) => fetch(base + path).then((r) => r.json());
  const poll = async <T>(probe: () => Promise<T | undefined>) => {
    for (const deadline = Date.now() + 15_000; Date.now() < deadline; await new Promise((r) => setTimeout(r, 20))) {
      const v = await probe();
      if (v !== undefined) return v;
    }
    throw new Error('timed out');
  };

  await fetch(`${base}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'review', requestId: 'r-1' }) });
  const [pending] = await poll(async () => { const { approvals } = await get('/approvals'); return approvals.length ? approvals : undefined; });
  await poll(async () => ((await get('/ping')).waitingOnHuman ? true : undefined));

  clock += 120_000;
  await server.sweep();
  assert.equal(driver.pid(key), undefined, 'a session waiting on a person is stopped');

  const verdict = await fetch(`${base}/approvals/${pending.id}`, { method: 'POST', body: JSON.stringify({ decision: 'approve', by: 'jf' }) });
  assert.equal(verdict.status, 200);
  assert.notEqual(driver.pid(key), undefined, 'the verdict restarted the session');
  const done = await fetch(`${base}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'review', requestId: 'r-1', wait: true }) }).then((r) => r.json());
  assert.equal(done.answer, 'Posted the review.');
  assert.equal(await readFile(join(dataDir, sessionDirName(key), 'posted'), 'utf8'), 'x', 'the gated tool ran once');
});

test('an invocation held past undici\'s headers timeout is still proxied, not turned into a 502', async () => {
  // The session sends no headers for 2 s; a fetch proxy with a 100 ms headers timeout gives up at undici's next 1 s timer tick.
  const session = createHttpServer((_req, res) => setTimeout(() => res.writeHead(200, { 'content-type': 'application/json' }).end('{"status":"answered"}'), 2_000)).listen(0);
  await new Promise((r) => session.once('listening', r));
  const url = `http://127.0.0.1:${(session.address() as AddressInfo).port}`;
  const driver = { agents: async () => [], agentOf: async () => undefined, rebind: async () => {}, agent: async () => undefined, promote: async () => 'draft' as const, pinned: async () => ({}), exported: async () => ({ transcript: [], artifacts: [] }), ensure: async () => url, stop: async () => {}, remove: async () => false, setWake: async () => {}, wakes: async () => [], running: () => [], pid: () => undefined, credential: () => undefined };
  const server = createDistributor(driver, { idleMs: 60_000 }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const previous = getGlobalDispatcher();
  setGlobalDispatcher(new Agent({ headersTimeout: 100 })); // restored in finally: later tests share this process
  try {
    const res = await ufetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/slow/invocations`, { method: 'POST', body: '{}', dispatcher: new Agent({ headersTimeout: 0 }) });
    assert.equal(res.status, 200);
    assert.deepEqual(await res.json(), { status: 'answered' });
  } finally {
    setGlobalDispatcher(previous);
    await Promise.all([new Promise((r) => server.close(r)), new Promise((r) => session.close(r))]);
  }
});

test('a client that hangs up mid-body does not take the distributor down', async () => {
  let upstreamClosed!: () => void;
  const closed = new Promise<void>((r) => (upstreamClosed = r));
  const session = createHttpServer((req, res) => {
    res.on('close', upstreamClosed);
    res.writeHead(200, { 'content-type': 'application/json' });
    res.write('{"partial":');
  }).listen(0);
  await new Promise((r) => session.once('listening', r));
  const url = `http://127.0.0.1:${(session.address() as AddressInfo).port}`;
  const driver = { agents: async () => [], agentOf: async () => undefined, rebind: async () => {}, agent: async () => undefined, promote: async () => 'draft' as const, pinned: async () => ({}), exported: async () => ({ transcript: [], artifacts: [] }), ensure: async () => url, stop: async () => {}, remove: async () => false, setWake: async () => {}, wakes: async () => [], running: () => [], pid: () => undefined, credential: () => undefined };
  const server = createDistributor(driver, { idleMs: 60_000 }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const port = (server.address() as AddressInfo).port;
  try {
    const req = httpRequest(`http://127.0.0.1:${port}/sessions/gone/invocations`, { method: 'POST' }, (res) => { res.on('error', () => {}); res.once('data', () => req.destroy()); });
    req.on('error', () => {}).end('{}');
    await closed;
    assert.equal((await fetch(`http://127.0.0.1:${port}/agents`)).status, 200);
  } finally {
    await Promise.all([new Promise((r) => server.close(r)), new Promise((r) => session.close(r))]);
  }
});

test('a session process gets an allowlisted environment, not the host\'s', async () => {
  process.env.PLANTED_SECRET = 'host-secret';
  try {
    const plain = await startDistributor(60_000);
    assert.equal((await plain.invoke('env-a', { input: 'secret', wait: true })).body.answer, 'secret: none');
    const declared = await startDistributor(60_000, { defaultAgent: '', inherit: () => ({ ...shellEnv(process.env), PLANTED_SECRET: process.env.PLANTED_SECRET }) });
    assert.equal((await declared.invoke('env-b', { input: 'secret', wait: true })).body.answer, 'secret: host-secret');
  } finally {
    delete process.env.PLANTED_SECRET;
  }
});

test('a launch hands the session its outbound credential as a 0600 file beside the listener URL, never as an env value', async () => {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-dist-'));
  dirs.push(dataDir);
  // The child comes up only when what it was handed checks out, so a wrong launch fails the start.
  const check = "const fs = require('fs'); const f = process.env.QUACK_LAUNCH_TOKEN_FILE; const ok = process.env.QUACK_SESSION_LISTENER_URL === 'http://listener' && (fs.statSync(f).mode & 0o777) === 0o600 && fs.readFileSync(f, 'utf8') === 'minted' && !Object.keys(process.env).some((k) => k.startsWith('QUACK_INDEX')); if (!ok) process.exit(3); process.send({ port: 1 }); setInterval(() => {}, 1000);";
  const driver = processDriver({ command: [process.execPath, '-e', check], dataDir, defaultAgent: '', env: { QUACK_SESSION_LISTENER_URL: 'http://listener' }, broker: { mint: () => 'minted', revoke: () => {} } });
  await driver.ensure('research:handed');
  await driver.stop('research:handed');
});

test('a session process that never reports a port fails with its output', async () => {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-dist-'));
  dirs.push(dataDir);
  const hang = "console.log('loading models'); setInterval(() => {}, 1000);";
  // Long enough that the first line has landed under a loaded runner before the start gives up.
  const driver = processDriver({ command: [process.execPath, '-e', hang], dataDir, defaultAgent: '', startTimeoutMs: 2_000 });
  await assert.rejects(driver.ensure('research:hung'), /did not report a port; output: loading models/);
});

test('a session process that ignores SIGTERM is killed at the stop deadline', async () => {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-dist-'));
  dirs.push(dataDir);
  const deaf = "process.on('SIGTERM', () => {}); process.send({ port: 1 }); setInterval(() => {}, 1000);";
  const deadlines: Array<{ fn: () => void; ms: number }> = [];
  const setTimer = (fn: () => void, ms: number) => (deadlines.push({ fn, ms }), () => {});
  const driver = processDriver({ command: [process.execPath, '-e', deaf], dataDir, defaultAgent: '', stopTimeoutMs: 5_000, setTimer });
  await driver.ensure('research:deaf');
  const pid = driver.pid('research:deaf')!;
  const stopped = driver.stop('research:deaf');
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(deadlines.map((d) => d.ms), [5_000]);
  deadlines[0].fn();
  await stopped;
  assert.throws(() => process.kill(pid, 0), /ESRCH/, 'the process is gone');
});

test('a session process exits when the parent holding its IPC channel goes away', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-orphan-'));
  dirs.push(dir);
  await writeFile(join(dir, 'token'), 't');
  const child = spawn(process.execPath, [fileURLToPath(new URL('./fixtures/echo-session.ts', import.meta.url))], { env: { ...process.env, QUACK_SESSION_DIR: dir, QUACK_SESSION_TOKEN_FILE: join(dir, 'token') }, stdio: ['ignore', 'inherit', 'inherit', 'ipc'] });
  await childPort(child, 'echo session', 15_000);
  const exited = new Promise((r) => child.once('exit', (code) => r(code)));
  child.disconnect();
  assert.equal(await exited, 0);
});

test('a broken output sink neither throws nor loses the tail', async () => {
  const src = new PassThrough();
  const dst = new Writable({ write: (_b, _e, cb) => cb(new Error('EPIPE')) });
  const seen: string[] = [];
  forwardOutput(src, dst, (b) => seen.push(String(b)));
  src.write('one');
  dst.destroy(new Error('EPIPE'));
  src.end('two');
  await new Promise((r) => src.once('end', r));
  assert.deepEqual(seen, ['one', 'two']);
});

test('session output reaches the shared sink line by line under its key, and an ended session leaves no listener on the sink', async () => {
  const dst = new PassThrough();
  const out: string[] = [];
  dst.on('data', (b) => out.push(String(b)));
  const before = [dst.listenerCount('error'), dst.listenerCount('close')];
  const src = new PassThrough();
  forwardOutput(src, dst, () => {}, '[k1] ');
  const accent = Buffer.from('é');
  src.write('one\ntwo ');
  src.write(accent.subarray(0, 1));
  src.end(Buffer.concat([accent.subarray(1), Buffer.from('\nlast')]));
  await new Promise((r) => src.once('close', r));
  assert.equal(out.join(''), '[k1] one\n[k1] two é\n[k1] last\n');
  assert.deepEqual([dst.listenerCount('error'), dst.listenerCount('close')], before);
});

test('the session hears the proxy\'s person or the tailnet as a principal beside its bearer, and an unproven identity is refused', async () => {
  const seen: string[] = [];
  const session = createHttpServer((req, res) => { seen.push(`${req.headers.authorization} ${decodeURIComponent(String(req.headers['x-quack-principal']))} ${req.headers['x-authentik-username']}`); res.writeHead(200, { 'content-type': 'application/json' }).end('{}'); }).listen(0);
  await new Promise((r) => session.once('listening', r));
  const url = `http://127.0.0.1:${(session.address() as AddressInfo).port}`;
  const driver = { agents: async () => [], agentOf: async () => '', rebind: async () => {}, agent: async () => undefined, promote: async () => 'draft' as const, pinned: async () => ({}), exported: async () => ({ transcript: [], artifacts: [] }), ensure: async () => url, stop: async () => {}, remove: async () => false, setWake: async () => {}, wakes: async () => [], running: () => [], pid: () => undefined, credential: () => 'launch-1' };
  const server = createDistributor(driver, { idleMs: 60_000, proxySecret: PROXY_SECRET }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/k1`;
  assert.equal((await fetch(`${base}/approvals/a`, { method: 'POST', body: '{}', headers: PERSON })).status, 200);
  assert.equal((await fetch(`${base}/approvals/a`, { method: 'POST', body: '{}', headers: { authorization: 'Bearer mine', 'x-quack-principal': 'forged' } })).status, 200);
  for (const forged of [{ 'x-authentik-username': 'jason' }, { 'x-authentik-username': 'jason', [PROXY_PROOF]: 'guess' }] as Array<Record<string, string>>) {
    const refused = await fetch(`${base}/approvals/a`, { method: 'POST', body: '{}', headers: forged });
    assert.equal(refused.status, 401, 'an identity without the proxy\'s proof');
  }
  assert.deepEqual(seen, ['Bearer launch-1 {"kind":"person","name":"jason"} undefined', 'Bearer launch-1 {"kind":"tailnet"} undefined']);
  await new Promise((r) => server.close(r));
  await new Promise((r) => session.close(r));
});
