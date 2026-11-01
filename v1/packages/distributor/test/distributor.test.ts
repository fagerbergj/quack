import assert from 'node:assert/strict';
import { createServer as createHttpServer } from 'node:http';
import { connect, type AddressInfo } from 'node:net';
import { cp, mkdir, mkdtemp, readFile, rm, utimes, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { nextFrame, runEnded } from '../../session/test/ws-helpers.ts';
import { createDistributor, processDriver, sessionDirName } from '../src/index.ts';

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
  const ranAs = (key: string) => `agent: ${join(dataDir, sessionDirName(key), 'agent-def', 'agent.json')}`;
  return { driver, invoke, base, ranAs, dataDir };
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
  const broken = { agents: async () => [], sessions: async () => [], ensure: async () => { throw new Error('no capacity'); }, stop: async () => {}, setWake: async () => {}, wakes: async () => [], running: () => [], pid: () => undefined };
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
  const { invoke, base, driver, ranAs } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  const whoami = (key: string, agent?: string) => invoke(key, { input: 'whoami', wait: true }, agent);
  assert.equal((await whoami('github:a#1', 'reviewer')).body.answer, ranAs('github:a#1'));
  assert.equal((await whoami('research:b', 'research')).body.answer, ranAs('research:b'));

  const { agents } = (await (await fetch(`${base}/agents`)).json()) as { agents: Array<{ name: string }> };
  assert.deepEqual(agents.sort((a, b) => a.name.localeCompare(b.name)), [{ name: 'research', version: 3, status: 'draft' }, { name: 'reviewer', version: 3, status: 'draft' }]);
  await driver.stop('research:b');
  const { sessions } = (await (await fetch(`${base}/sessions`)).json()) as { sessions: Array<{ key: string; agent: string; running: boolean; lastActivity: string }> };
  assert.deepEqual(sessions.map((s) => [s.key, s.agent, s.running]).sort(), [['github:a#1', 'reviewer', true], ['research:b', 'research', false]]);
  assert.ok(sessions.every((s) => !Number.isNaN(Date.parse(s.lastActivity))));
});

test('lastActivity comes from the session log, so file mtimes from a restart do not move it', async () => {
  const { invoke, base, driver, dataDir } = await startDistributor(60_000);
  await invoke('act:1', { input: 'hi', wait: true });
  await driver.stop('act:1');
  const read = async () => ((await (await fetch(`${base}/sessions`)).json()) as { sessions: Array<{ lastActivity: string }> }).sessions[0]!.lastActivity;
  const before = await read();
  const future = new Date('2099-01-01T00:00:00Z');
  const dir = join(dataDir, sessionDirName('act:1'));
  await Promise.all(['session.sqlite', 'session-key'].map((f) => utimes(join(dir, f), future, future)));
  assert.equal(await read(), before);
  assert.ok(Date.parse(before) < future.getTime());
});

test('a session keeps its agent: a different one is 409, an unknown one 404', async () => {
  const dir = await agentsDir('reviewer', 'research');
  const { invoke, ranAs } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  await invoke('research:x', { input: 'hi', wait: true }, 'research');
  assert.equal((await invoke('research:x', { input: 'hi', wait: true }, 'reviewer')).status, 409);
  assert.equal((await invoke('research:x', { input: 'hi', wait: true }, 'research')).status, 200);
  assert.equal((await invoke('research:x', { input: 'whoami', wait: true })).body.answer, ranAs('research:x'));
  assert.equal((await invoke('research:y', { input: 'hi' }, 'nope')).status, 404);
});

test('a session naming no agent gets the deployment default', async () => {
  const dir = await agentsDir('reviewer', 'research');
  const defaultAgent = join(dir, 'reviewer', 'agent.json');
  const { invoke, base, ranAs } = await startDistributor(60_000, { agentsDir: dir, defaultAgent });
  assert.equal((await invoke('github:a#2', { input: 'whoami', wait: true })).body.answer, ranAs('github:a#2'));
  assert.equal((await invoke('github:a#2', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  assert.equal((await invoke('github:a#2', { input: 'hi', wait: true }, 'research')).status, 409);
  const { sessions } = (await (await fetch(`${base}/sessions`)).json()) as { sessions: Array<{ agent: string }> };
  assert.equal(sessions[0].agent, 'reviewer');
});

test('a registry agent sharing the default agent\'s name runs the registry file', async () => {
  const dir = await agentsDir('reviewer');
  const other = await agentsDir('reviewer');
  const { invoke, ranAs } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: join(other, 'reviewer', 'agent.json') });
  assert.equal((await invoke('github:a#3', { input: 'whoami', wait: true }, 'reviewer')).body.answer, ranAs('github:a#3'));
});

test('a session stays on the version it started on after the agents dir moves on; a new one gets the newest', async () => {
  const dir = await agentsDirAt(1, 'reviewer');
  const { invoke, driver, base, ranAs } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  await invoke('github:v#1', { input: 'hi', wait: true }, 'reviewer');
  await driver.stop('github:v#1');
  await agentsDirAt(2, 'reviewer').then((next) => cp(join(next, 'reviewer'), join(dir, 'reviewer'), { recursive: true }));

  assert.equal((await invoke('github:v#1', { input: 'whoami', wait: true })).body.answer, ranAs('github:v#1'));
  assert.equal((await invoke('github:v#2', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  const pinned = async (key: string) => JSON.parse(await readFile(ranAs(key).slice('agent: '.length), 'utf8')).version;
  assert.deepEqual([await pinned('github:v#1'), await pinned('github:v#2')], [1, 2]);
  const prompt = (key: string) => readFile(join(ranAs(key).slice('agent: '.length), '..', 'prompt.md'), 'utf8');
  assert.deepEqual([await prompt('github:v#1'), await prompt('github:v#2')], ['prompt v1', 'prompt v2']);
  const { sessions } = (await (await fetch(`${base}/sessions`)).json()) as { sessions: Array<{ key: string; version: number }> };
  assert.deepEqual(sessions.map((x) => [x.key, x.version]).sort(), [['github:v#1', 1], ['github:v#2', 2]]);
});

test('an agent version whose content changed is refused for new sessions but not existing ones', async () => {
  const dir = await agentsDirAt(1, 'reviewer');
  const { invoke } = await startDistributor(60_000, { agentsDir: dir, defaultAgent: '' });
  assert.equal((await invoke('github:f#1', { input: 'hi', wait: true }, 'reviewer')).status, 200);
  await writeFile(join(dir, 'reviewer', 'prompt.md'), 'edited without a version bump');
  assert.equal((await invoke('github:f#2', { input: 'hi', wait: true }, 'reviewer')).status, 409);
  assert.equal((await invoke('github:f#1', { input: 'hi', wait: true })).status, 200);
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
  const stub = createHttpServer((_req, res) => res.end(JSON.stringify(ping(clock)))).listen(0);
  await new Promise((r) => stub.once('listening', r));
  servers.push(() => new Promise((r) => stub.close(() => r())));
  const url = `http://127.0.0.1:${(stub.address() as AddressInfo).port}`;
  const wakes = new Map<string, number>();
  const running = new Set<string>();
  const log: string[] = [];
  const driver = {
    agents: async () => [], sessions: async () => [], pid: () => undefined,
    ensure: async (key: string) => { if (!running.has(key)) log.push(`start ${key}`); running.add(key); return url; },
    stop: async (key: string) => { running.delete(key); log.push(`stop ${key}`); },
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

test('a restarted distributor rebuilds wake timers from the recorded times', async () => {
  const fleet = await fakeFleet(() => ({ status: 'Healthy', wake: {} }));
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
