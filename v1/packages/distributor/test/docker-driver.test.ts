import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { credentialBroker, containerEnv, dockerApi, dockerLauncher, parseDockerHost, parseHostConfig, reapSessions, sessionDirName, sessionHost, type DockerCall } from '../src/index.ts';

const cleanups: Array<() => void | Promise<void>> = [];
after(async () => {
  for (const fn of cleanups) await fn();
});

async function until(probe: () => boolean, ms = 15_000) {
  const deadline = Date.now() + ms;
  while (!probe()) {
    if (Date.now() > deadline) throw new Error('timed out');
    await new Promise((r) => setTimeout(r, 20));
  }
}

const listen = async (handler: Parameters<typeof createServer>[1]) => {
  const server = createServer(handler).listen(0, '127.0.0.1');
  await new Promise((r) => server.once('listening', r));
  cleanups.push(() => void server.close());
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
};

type Fake = { name: string; body: Record<string, any>; started: boolean; exit: () => void };

// An in-memory Docker Engine: containers are records, and the session each one runs is a local server answering /ping.
function fakeDocker(sessionUrl: string) {
  const containers = new Map<string, Fake>();
  const calls: string[] = [];
  const waiters = new Map<string, Array<() => void>>();
  const api: DockerCall = async (method, path, body) => {
    calls.push(`${method} ${path}`);
    const [route] = path.split('?');
    const name = route.split('/')[2];
    if (method === 'POST' && route === '/containers/create') {
      const created = new URL(path, 'http://d').searchParams.get('name')!;
      containers.set(created, { name: created, body: body as Record<string, any>, started: false, exit: () => { containers.delete(created); waiters.get(created)?.forEach((f) => f()); } });
      return { status: 201, body: { Id: created } };
    }
    if (method === 'POST' && route.endsWith('/start')) return containers.get(name)!.started = true, { status: 204, body: undefined };
    if (method === 'POST' && route.endsWith('/wait')) {
      await new Promise<void>((r) => waiters.set(name, [...(waiters.get(name) ?? []), r]));
      return { status: 200, body: { StatusCode: 0 } };
    }
    if (method === 'POST' && route.endsWith('/stop')) return containers.get(name)?.exit(), { status: 204, body: undefined };
    if (method === 'DELETE') return containers.get(name)?.exit(), { status: containers.has(name) ? 204 : 404, body: undefined };
    if (method === 'GET' && route === '/containers/json') return { status: 200, body: [...containers.values()].map((c) => ({ Id: c.name })) };
    return { status: 400, body: undefined };
  };
  return { api, containers, calls, sessionUrl };
}

async function setup(inheritedEnv: NodeJS.ProcessEnv = {}, defaultAgent?: string) {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-docker-'));
  cleanups.push(() => rm(dataDir, { recursive: true, force: true }));
  const sessionUrl = await listen((_req, res) => res.end('{}'));
  const docker = fakeDocker(sessionUrl);
  const broker = credentialBroker();
  const launch = dockerLauncher({ api: docker.api, broker, image: 'quack:test', runtime: 'runsc', network: 'quack-sessions', hostDataDir: '/srv/quack/data', distributorUrl: 'http://quack-v1:8080', inherit: () => inheritedEnv, urlFor: () => sessionUrl, sleep: async () => {}, startTimeoutMs: 1000 });
  const driver = sessionHost({ dataDir, defaultAgent }, launch);
  return { driver, docker, broker, dataDir };
}

test('a session starts in its own gVisor container that mounts only its own directory', async () => {
  const { driver, docker, dataDir } = await setup();
  await mkdir(join(dataDir, sessionDirName('other')), { recursive: true });
  assert.equal(await driver.ensure('a'), docker.sessionUrl);
  const [c] = [...docker.containers.values()];
  assert.equal(c.name, `quack-session-${sessionDirName('a')}`);
  assert.equal(c.started, true);
  assert.equal(c.body.Image, 'quack:test');
  assert.equal(c.body.HostConfig.Runtime, 'runsc');
  assert.equal(c.body.HostConfig.NetworkMode, 'quack-sessions');
  assert.deepEqual(c.body.HostConfig.Binds, [`/srv/quack/data/${sessionDirName('a')}:/session`]);
  assert.deepEqual(c.body.HostConfig.CapDrop, ['ALL']);
  assert.ok(c.body.Env.includes('QUACK_SESSION_DIR=/session') && c.body.Env.includes('QUACK_SESSION_KEY=a'));
  assert.equal(await driver.ensure('a'), docker.sessionUrl, 'a running session is reused');
  assert.equal(docker.containers.size, 1);
  assert.deepEqual(driver.running(), ['a']);
});

test('stop stops the container and lets the session start again; a crash restarts it', async () => {
  const { driver, docker } = await setup();
  await driver.ensure('a');
  await driver.stop('a');
  assert.deepEqual(driver.running(), []);
  assert.equal(docker.containers.size, 0);
  assert.ok(docker.calls.some((c) => c.startsWith('POST /containers/quack-session-') && c.includes('/stop?t=')));
  await driver.ensure('a');
  assert.equal(docker.containers.size, 1, 'restarted after a stop');
  const first = [...docker.containers.values()][0];
  first.exit();
  await until(() => docker.containers.size === 1 && driver.running().includes('a') && docker.containers.get(first.name) !== first);
});

test('the container gets no service secrets: pairs are proxied, other secrets are dropped', async () => {
  const secret = { QUACK_GH_SERVICE_URL: 'http://gh:1', QUACK_GH_SERVICE_TOKEN: 'long-lived-gh', QUACK_EXA_API_KEY: 'exa-secret', QUACK_ZAI_API_KEY: 'zai-secret', PATH: '/usr/bin' };
  const { driver, docker, broker } = await setup(secret);
  await driver.ensure('a');
  const env: string[] = [...docker.containers.values()][0].body.Env;
  const text = env.join('\n');
  for (const leaked of ['long-lived-gh', 'exa-secret', 'zai-secret']) assert.ok(!text.includes(leaked), `${leaked} must not reach the container`);
  assert.ok(env.includes('QUACK_GH_SERVICE_URL=http://quack-v1:8080/svc/QUACK_GH_SERVICE'));
  assert.ok(env.includes('PATH=/usr/bin'));
  assert.ok(broker.has('QUACK_GH_SERVICE'));
});

test('an operator can pass a named secret through', () => {
  const broker = credentialBroker();
  const { env } = containerEnv({ QUACK_EXA_API_KEY: 'exa' }, { key: 'k', token: 't', broker, distributorUrl: 'http://d', passthrough: ['QUACK_EXA_API_KEY'], alwaysProxied: [], dir: '/data/x' });
  assert.equal(env.QUACK_EXA_API_KEY, 'exa');
});

test('the pinned agent definition and deployment config are addressed inside the container', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-cfg-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  await writeFile(join(dir, 'deploy.json'), '{"providers":{}}');
  await writeFile(join(dir, 'agent.json'), JSON.stringify({ name: 'echo', version: 1, extensions: {} }));
  const { driver, docker, dataDir } = await setup({ QUACK_CONFIG: join(dir, 'deploy.json') }, join(dir, 'agent.json'));
  await driver.ensure('a');
  const env: string[] = [...docker.containers.values()][0].body.Env;
  assert.ok(env.includes('QUACK_CONFIG=/session/deployment.json'));
  assert.ok(env.includes('QUACK_AGENT=/session/agent-def/agent.json'));
  assert.equal(await readFile(join(dataDir, sessionDirName('a'), 'deployment.json'), 'utf8'), '{"providers":{}}');
});

test('the broker forwards with the real token for a live session token and refuses others', async () => {
  let seen = '';
  const upstream = await listen((req, res) => {
    seen = `${req.method} ${req.url} ${req.headers.authorization}`;
    res.end('{"ok":true}');
  });
  let clock = 0;
  const broker = credentialBroker({ ttlMs: 1000, now: () => clock });
  broker.register('QUACK_SLEEPER', upstream, 'long-lived');
  const front = await listen((req, res) => void broker.route(req, res).then((handled) => handled || res.writeHead(404).end()));
  const token = broker.mint('a');
  assert.equal((await fetch(`${front}/svc/QUACK_SLEEPER/predict?x=1`, { headers: { authorization: `Bearer ${token}` } })).status, 200);
  assert.equal(seen, 'GET /predict?x=1 Bearer long-lived');
  assert.equal((await fetch(`${front}/svc/QUACK_SLEEPER/predict`, { headers: { authorization: 'Bearer long-lived' } })).status, 401);
  assert.equal((await fetch(`${front}/svc/NOPE/x`, { headers: { authorization: `Bearer ${token}` } })).status, 404);
  clock = 1000;
  assert.equal((await fetch(`${front}/svc/QUACK_SLEEPER/predict`, { headers: { authorization: `Bearer ${token}` } })).status, 401, 'expired');
});

test('a stopped session loses its token', async () => {
  const { driver, broker } = await setup();
  const minted: string[] = [];
  const mint = broker.mint;
  broker.mint = (key) => (minted.push(mint(key)), minted.at(-1)!);
  await driver.ensure('a');
  const front = await listen((req, res) => void broker.route(req, res).then(() => res.writableEnded || res.end()));
  broker.register('QUACK_X', front, 't');
  const status = async () => (await fetch(`${front}/svc/QUACK_X/`, { headers: { authorization: `Bearer ${minted[0]}` } })).status;
  assert.notEqual(await status(), 401);
  await driver.stop('a');
  assert.equal(await status(), 401);
});

test('a container that exits while starting fails the start and leaves no token', async () => {
  const { docker, broker } = await setup();
  const dead: DockerCall = async (method, path, body) => {
    const res = await docker.api(method, path, body);
    if (path.endsWith('/start')) [...docker.containers.values()][0].exit();
    return res;
  };
  const dir = await mkdtemp(join(tmpdir(), 'quack-dead-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  const launch = dockerLauncher({ api: dead, broker, image: 'i', runtime: 'runsc', network: 'n', hostDataDir: '/h', distributorUrl: 'http://d', inherit: () => ({}), urlFor: () => 'http://127.0.0.1:9', sleep: async () => {}, startTimeoutMs: 1000 });
  await assert.rejects(launch({ key: 'a', dir: join(dir, 'abc'), agentPath: undefined }), /exited during start/);
});

test('reapSessions removes containers a previous distributor left', async () => {
  const { docker } = await setup();
  await docker.api('POST', '/containers/create?name=old', {});
  await reapSessions(docker.api);
  assert.equal(docker.containers.size, 0);
});

test('QUACK_SESSION_DRIVER defaults to process and docker needs its settings', () => {
  assert.deepEqual(parseHostConfig({}), { driver: 'process' });
  assert.throws(() => parseHostConfig({ QUACK_SESSION_DRIVER: 'kata' }), /process or docker/);
  assert.throws(() => parseHostConfig({ QUACK_SESSION_DRIVER: 'docker' }), /QUACK_HOST_DATA_DIR is required/);
  const ok = { QUACK_SESSION_DRIVER: 'docker', QUACK_HOST_DATA_DIR: '/srv/d', QUACK_SESSION_DISTRIBUTOR_URL: 'http://quack-v1:8080/', QUACK_SESSION_IMAGE: 'quack:1' };
  assert.deepEqual(parseHostConfig(ok), { driver: 'docker', runtime: 'runsc', hostDataDir: '/srv/d', distributorUrl: 'http://quack-v1:8080', image: 'quack:1', network: 'quack-sessions', dockerHost: '/var/run/docker.sock', passthrough: [] });
  assert.throws(() => parseHostConfig({ ...ok, QUACK_HOST_DATA_DIR: 'data' }), /absolute/);
  assert.throws(() => parseHostConfig({ ...ok, QUACK_SESSION_RUNTIME: 'runc' }), /QUACK_ALLOW_RUNC/);
  assert.equal((parseHostConfig({ ...ok, QUACK_SESSION_RUNTIME: 'runc', QUACK_ALLOW_RUNC: '1' }) as { runtime: string }).runtime, 'runc');
});

test('dockerApi speaks JSON to a TCP host such as a socket proxy', async () => {
  const url = await listen((req, res) => {
    let body = '';
    req.on('data', (c) => (body += c)).on('end', () => res.writeHead(201, { 'content-type': 'application/json' }).end(JSON.stringify({ got: `${req.method} ${req.url} ${body}` })));
  });
  const res = await dockerApi(parseDockerHost(url.replace('http', 'tcp')))('POST', '/containers/create?name=x', { a: 1 });
  assert.deepEqual(res, { status: 201, body: { got: 'POST /containers/create?name=x {"a":1}' } });
  assert.deepEqual(parseDockerHost('unix:///var/run/docker.sock'), { socketPath: '/var/run/docker.sock' });
});
