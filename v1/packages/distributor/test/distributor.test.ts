import assert from 'node:assert/strict';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { connect, type AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { nextFrame, runEnded } from '../../session/test/ws-helpers.ts';
import { createDistributor, processDriver } from '../src/index.ts';

const dirs: string[] = [];
const servers: Array<() => Promise<void>> = [];
after(async () => {
  await Promise.all(servers.map((close) => close()));
  await Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true })));
});

// Each test gets its own distributor so idle reaping in one cannot stop another's session.
async function agentsDir(...names: string[]) {
  const dir = await mkdtemp(join(tmpdir(), 'quack-agents-'));
  dirs.push(dir);
  for (const name of names) {
    await mkdir(join(dir, name));
    await writeFile(join(dir, name, 'agent.json'), JSON.stringify({ name, version: 3 }));
  }
  return dir;
}

async function startDistributor(idleMs: number, agents: { agentsDir?: string; defaultAgent?: string } = { defaultAgent: '' }) {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-dist-'));
  dirs.push(dataDir);
  const driver = processDriver({ command: [process.execPath, fileURLToPath(new URL('./fixtures/echo-session.ts', import.meta.url))], dataDir, ...agents });
  const server = createDistributor(driver, { idleMs }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  servers.push(async () => {
    await Promise.all(driver.running().map((k) => driver.stop(k)));
    await new Promise((r) => server.close(r));
  });
  const invoke = (key: string, body: unknown, agent?: string) =>
    fetch(`${base}/sessions/${encodeURIComponent(key)}/invocations${agent ? `?agent=${agent}` : ''}`, { method: 'POST', body: JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json() }));
  return { driver, invoke, base };
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
  const { driver, invoke } = await startDistributor(300);
  await invoke('research:spec-decoding', { input: 'first', wait: true });
  await until(() => driver.running().length === 0);
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

test('proxies the live stream and keeps a watched session from idling out', async () => {
  const { driver, base } = await startDistributor(200);
  const key = 'github:fagerbergj/quack#1827';
  const ws = new WebSocket(`${base.replace('http', 'ws')}/sessions/${encodeURIComponent(key)}/ws`);
  assert.equal((await nextFrame(ws, () => true)).type, 'snapshot');

  const ended = nextFrame(ws, runEnded);
  ws.send(JSON.stringify({ type: 'input', input: 'hi', requestId: 'w-1' }));
  assert.match(JSON.stringify((await ended).events), /echo: hi/);

  // Several idle sweeps pass while the viewer is connected; the session must still be running.
  await new Promise((r) => setTimeout(r, 1_000));
  assert.deepEqual(driver.running(), [key]);
  ws.close();
  await until(() => driver.running().length === 0);
});

test('a viewer whose session cannot start is disconnected, and the distributor keeps serving', async () => {
  const broken = { agents: async () => [], sessions: async () => [], ensure: async () => { throw new Error('no capacity'); }, stop: async () => {}, running: () => [], pid: () => undefined };
  const server = createDistributor(broken, { idleMs: 60_000 }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const ws = new WebSocket(`${base.replace('http', 'ws')}/sessions/research%3Ax/ws`);
  const closed = await new Promise<string>((r) => { ws.onerror = () => r('error'); ws.onopen = () => r('open'); });
  assert.equal(closed, 'error');
  assert.equal((await fetch(`${base}/sessions`)).status, 200);
  await new Promise((r) => server.close(r));
});

test('each session runs on the agent that created it, and the registry and index list them', async () => {
  const dir = await agentsDir('reviewer', 'research');
  const { invoke, base, driver } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  const whoami = (key: string, agent?: string) => invoke(key, { input: 'whoami', wait: true }, agent);
  assert.equal((await whoami('github:a#1', 'reviewer')).body.answer, `agent: ${join(dir, 'reviewer', 'agent.json')}`);
  assert.equal((await whoami('research:b', 'research')).body.answer, `agent: ${join(dir, 'research', 'agent.json')}`);

  const { agents } = (await (await fetch(`${base}/agents`)).json()) as { agents: Array<{ name: string }> };
  assert.deepEqual(agents.sort((a, b) => a.name.localeCompare(b.name)), [{ name: 'research', version: 3 }, { name: 'reviewer', version: 3 }]);
  await driver.stop('research:b');
  const { sessions } = (await (await fetch(`${base}/sessions`)).json()) as { sessions: Array<{ key: string; agent: string; running: boolean; lastActivity: string }> };
  assert.deepEqual(sessions.map((s) => [s.key, s.agent, s.running]).sort(), [['github:a#1', 'reviewer', true], ['research:b', 'research', false]]);
  assert.ok(sessions.every((s) => !Number.isNaN(Date.parse(s.lastActivity))));
});

test('a session keeps its agent: a different one is 409, an unknown one 404', async () => {
  const dir = await agentsDir('reviewer', 'research');
  const { invoke } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  await invoke('research:x', { input: 'hi', wait: true }, 'research');
  assert.equal((await invoke('research:x', { input: 'hi', wait: true }, 'reviewer')).status, 409);
  assert.equal((await invoke('research:x', { input: 'hi', wait: true }, 'research')).status, 200);
  assert.equal((await invoke('research:x', { input: 'whoami', wait: true })).body.answer, `agent: ${join(dir, 'research', 'agent.json')}`);
  assert.equal((await invoke('research:y', { input: 'hi' }, 'nope')).status, 404);
});

test('a session naming no agent gets the deployment default', async () => {
  const dir = await agentsDir('reviewer', 'research');
  const defaultAgent = join(dir, 'reviewer', 'agent.json');
  const { invoke, base } = await startDistributor(60_000, { agentsDir: dir, defaultAgent });
  assert.equal((await invoke('github:a#2', { input: 'whoami', wait: true })).body.answer, `agent: ${defaultAgent}`);
  assert.equal((await invoke('github:a#2', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  assert.equal((await invoke('github:a#2', { input: 'hi', wait: true }, 'research')).status, 409);
  const { sessions } = (await (await fetch(`${base}/sessions`)).json()) as { sessions: Array<{ agent: string }> };
  assert.equal(sessions[0].agent, 'reviewer');
});

test('a registry agent sharing the default agent\'s name runs the registry file', async () => {
  const dir = await agentsDir('reviewer');
  const other = await agentsDir('reviewer');
  const { invoke } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: join(other, 'reviewer', 'agent.json') });
  assert.equal((await invoke('github:a#3', { input: 'whoami', wait: true }, 'reviewer')).body.answer, `agent: ${join(dir, 'reviewer', 'agent.json')}`);
});

test('QUACK_WEB_DIR serves files, refuses traversal, and leaves API routes alone', async () => {
  const web = await mkdtemp(join(tmpdir(), 'quack-web-'));
  dirs.push(web);
  await mkdir(join(web, 'assets'));
  await writeFile(join(web, 'index.html'), '<h1>quack</h1>');
  await writeFile(join(web, 'assets', 'app.js'), 'x');
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
  assert.equal((await fetch(`${base}/assets/`)).status, 404);
  assert.equal((await fetch(`${base}/missing.js`)).status, 404);
  // fetch normalizes dot segments, so the traversal goes out as a raw request line.
  const raw = await new Promise<string>((res) => {
    const s = connect(port, '127.0.0.1', () => s.write('GET /%2e%2e/%2e%2e/etc/passwd HTTP/1.1\r\nhost: x\r\nconnection: close\r\n\r\n'));
    let out = '';
    s.on('data', (d) => (out += d)).on('close', () => res(out));
  });
  assert.match(raw, /^HTTP\/1.1 404/);
  assert.deepEqual(await (await fetch(`${base}/sessions`)).json(), { sessions: [] });
  assert.deepEqual(await (await fetch(`${base}/agents`)).json(), { agents: [] });
});

test('without QUACK_WEB_DIR nothing is served', async () => {
  const { base } = await startDistributor(60_000);
  assert.equal((await fetch(`${base}/`)).status, 404);
  assert.equal((await fetch(`${base}/index.html`)).status, 404);
});
