import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { createServer as createTlsServer } from 'node:https';
import type { AddressInfo } from 'node:net';
import { tmpdir, type NetworkInterfaceInfo } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { credentialBroker, containerEnv, dockerApi, dockerLauncher, onSessionNetwork, parseDockerHost, parseHostConfig, reapSessions, sessionDirName, sessionHost, type DockerCall } from '../src/index.ts';
import { proxyProviders } from '../src/keyed-services.ts';
import { sessionDriver } from '../src/session-driver.ts';

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

async function setup(inheritedEnv: NodeJS.ProcessEnv = {}, defaultAgent?: string, egressProxy?: string) {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-docker-'));
  cleanups.push(() => rm(dataDir, { recursive: true, force: true }));
  const sessionUrl = await listen((_req, res) => res.end('{}'));
  const docker = fakeDocker(sessionUrl);
  const broker = credentialBroker();
  const launch = dockerLauncher({ api: docker.api, broker, image: 'quack:test', runtime: 'runsc', network: 'quack-sessions', data: { hostDir: '/srv/quack/data' }, distributorUrl: 'http://quack-v1:8080', egressProxy, inherit: () => inheritedEnv, urlFor: () => sessionUrl, sleep: async () => {}, startTimeoutMs: 1000 });
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

test('the container gets no secrets: keyed services are proxied, the distributor is reached with the launch token, the rest is dropped', async () => {
  const secret = { QUACK_GH_SERVICE_URL: 'http://gh:1', QUACK_GH_SERVICE_TOKEN: 'long-lived-gh', QUACK_EXA_API_KEY: 'exa-secret', LANGFUSE_BASE_URL: 'http://lf', LANGFUSE_PUBLIC_KEY: 'pk-real', LANGFUSE_SECRET_KEY: 'sk-real', QUACK_STRAY_API_KEY: 'stray-secret', PATH: '/usr/bin' };
  const { driver, docker, broker } = await setup(secret);
  await driver.ensure('a');
  const env: string[] = [...docker.containers.values()][0].body.Env;
  const text = env.join('\n');
  for (const leaked of ['long-lived-gh', 'exa-secret', 'pk-real', 'sk-real', 'stray-secret']) assert.ok(!text.includes(leaked), `${leaked} must not reach the container`);
  assert.ok(env.includes('QUACK_SESSION_LISTENER_URL=http://quack-v1:8080') && env.includes('QUACK_LAUNCH_TOKEN_FILE=/session/.listener-token'));
  assert.ok(!text.includes('QUACK_INDEX'), 'the launch credential is a file, never an env value');
  assert.ok(!text.includes('QUACK_GH_SERVICE'), 'an inherited service pair is no longer registered');
  assert.ok(env.includes('PATH=/usr/bin'));
  assert.ok(env.includes('QUACK_EXA_URL=http://quack-v1:8080/proxy/QUACK_EXA') && env.includes('LANGFUSE_BASE_URL=http://quack-v1:8080/proxy/LANGFUSE'));
  assert.ok(broker.has('QUACK_EXA') && broker.has('LANGFUSE') && !broker.has('QUACK_INDEX'));
});

test('a session with a named data volume mounts only its subpath of it', async () => {
  const { docker, broker } = await setup();
  const dir = await mkdtemp(join(tmpdir(), 'quack-vol-'));
  await mkdir(join(dir, 'abc'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  const launch = dockerLauncher({ api: docker.api, broker, image: 'i', runtime: 'runsc', network: 'n', data: { volume: 'quack_quack-v1-data' }, distributorUrl: 'http://d', inherit: () => ({}), urlFor: () => docker.sessionUrl, sleep: async () => {}, startTimeoutMs: 1000 });
  await launch({ key: 'a', dir: join(dir, 'abc'), agentPath: undefined });
  const { HostConfig } = [...docker.containers.values()][0].body;
  assert.deepEqual(HostConfig.Mounts, [{ Type: 'volume', Source: 'quack_quack-v1-data', Target: '/session', VolumeOptions: { Subpath: 'abc' } }]);
  assert.equal(HostConfig.Binds, undefined);
});

test('every provider is routed through the broker, keyed or not, and only keyed ones get the real key upstream', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-prov-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  const cfg = join(dir, 'deploy.json');
  await writeFile(cfg, JSON.stringify({ providers: { hosted: { baseUrl: 'https://llm.example/v1', models: {} }, local: { baseUrl: 'http://swap:1/v1', models: {} } } }));
  const { driver, docker, dataDir } = await setup({ QUACK_CONFIG: cfg, QUACK_HOSTED_API_KEY: 'hosted-secret' });
  const warnings: string[] = [];
  const warn = console.warn;
  console.warn = (m: string) => void warnings.push(m);
  await driver.ensure('a').finally(() => (console.warn = warn));
  assert.deepEqual(warnings, [], 'a provider key the driver replaced with the token is not reported as dropped');
  const env: string[] = [...docker.containers.values()][0].body.Env;
  assert.ok(!env.join('\n').includes('hosted-secret'));
  assert.ok(env.some((e) => /^QUACK_HOSTED_API_KEY=[0-9a-f]{64}$/.test(e)));
  const written = JSON.parse(await readFile(join(dataDir, sessionDirName('a'), 'deployment.json'), 'utf8'));
  assert.equal(written.providers.hosted.baseUrl, 'http://quack-v1:8080/proxy/PROVIDER_hosted');
  assert.equal(written.providers.local.baseUrl, 'http://quack-v1:8080/proxy/PROVIDER_local');
  assert.ok(env.some((e) => /^QUACK_LOCAL_API_KEY=[0-9a-f]{64}$/.test(e)), 'a keyless provider still gets the token, which the session sends as its key');
});

test('provider ids that sanitize alike get separate routes', () => {
  const { config } = proxyProviders({ providers: { 'a.b': { baseUrl: 'http://one' }, a_b: { baseUrl: 'http://two' } } }, {}, { broker: credentialBroker(), distributorUrl: 'http://d', token: 't' });
  assert.deepEqual(Object.values(config.providers).map((p) => p.baseUrl), ['http://d/proxy/PROVIDER_a_b', 'http://d/proxy/PROVIDER_a_b_2']);
});

test('an operator can pass a named secret through', () => {
  const broker = credentialBroker();
  const { env } = containerEnv({ QUACK_EXA_API_KEY: 'exa' }, { key: 'k', token: 't', broker, distributorUrl: 'http://d', passthrough: ['QUACK_EXA_API_KEY'], dir: '/data/x' });
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
    seen = `${req.method} ${req.url} ${req.headers.authorization} ${req.headers['x-api-key']}`;
    res.end('{"ok":true}');
  });
  const broker = credentialBroker();
  broker.register('PROVIDER_x', upstream, 'long-lived');
  const front = await listen((req, res) => void broker.route(req, res).then((handled) => handled || res.writeHead(404).end()));
  const token = broker.mint('a');
  assert.equal((await fetch(`${front}/proxy/PROVIDER_x/predict?x=1`, { headers: { authorization: `Bearer ${token}` } })).status, 200);
  assert.equal(seen, 'GET /predict?x=1 Bearer long-lived undefined');
  assert.equal((await fetch(`${front}/proxy/PROVIDER_x/predict`, { headers: { authorization: 'Bearer long-lived' } })).status, 401);
  broker.register('QUACK_EXA', upstream, { 'x-api-key': 'real-exa' });
  assert.equal((await fetch(`${front}/proxy/QUACK_EXA/search`, { method: 'POST', headers: { 'x-api-key': token, 'content-type': 'application/json' }, body: '{}' })).status, 200);
  assert.equal(seen, 'POST /search undefined real-exa');
  broker.register('LANGFUSE', upstream, { authorization: 'Basic real' });
  const basic = Buffer.from(`session:${token}`).toString('base64');
  await fetch(`${front}/proxy/LANGFUSE/api/x`, { method: 'POST', headers: { authorization: `Basic ${basic}` }, body: '{}' });
  assert.equal(seen, 'POST /api/x Basic real undefined');
  assert.equal((await fetch(`${front}/proxy/NOPE/x`, { headers: { authorization: `Bearer ${token}` } })).status, 404);
  broker.revoke(token);
  assert.equal((await fetch(`${front}/proxy/PROVIDER_x/predict`, { headers: { authorization: `Bearer ${token}` } })).status, 401, 'revoked');
});

test('the broker reaches http and https upstreams alike, trusting a CA it is given', async () => {
  const tls = { cert: await readFile(new URL('./fixtures/tls/cert.pem', import.meta.url)), key: await readFile(new URL('./fixtures/tls/key.pem', import.meta.url)) };
  const seen: string[] = [];
  const answer: Parameters<typeof createServer>[1] = (req, res) => { seen.push(`${req.method} ${req.url} ${req.headers['x-api-key']}`); res.end('{"ok":true}'); };
  const secure = createTlsServer(tls, answer).listen(0, '127.0.0.1');
  await new Promise((r) => secure.once('listening', r));
  cleanups.push(() => void secure.close());
  const plain = await listen(answer);
  const broker = credentialBroker({ ca: tls.cert });
  broker.register('QUACK_EXA', `https://127.0.0.1:${(secure.address() as AddressInfo).port}`, { 'x-api-key': 'real-exa' });
  broker.register('LANGFUSE', plain, { 'x-api-key': 'real-lf' });
  const front = await listen((req, res) => void broker.route(req, res));
  const token = broker.mint('a');
  const call = (name: string) => fetch(`${front}/proxy/${name}/search`, { method: 'POST', headers: { 'x-api-key': token }, body: '{}' }).then(async (r) => [r.status, await r.json()]);
  assert.deepEqual(await call('QUACK_EXA'), [200, { ok: true }]);
  assert.deepEqual(await call('LANGFUSE'), [200, { ok: true }]);
  assert.deepEqual(seen, ['POST /search real-exa', 'POST /search real-lf']);
  // Without the CA the self-signed upstream is refused, as an untrusted certificate must be.
  const untrusting = credentialBroker();
  untrusting.register('QUACK_EXA', `https://127.0.0.1:${(secure.address() as AddressInfo).port}`, {});
  const strict = await listen((req, res) => void untrusting.route(req, res));
  assert.equal((await fetch(`${strict}/proxy/QUACK_EXA/search`, { headers: { 'x-api-key': untrusting.mint('a') } })).status, 502);
});

test('a request body over 1 MiB reaches the upstream intact, and the upstream\'s own status passes back', async () => {
  const received: Buffer[] = [];
  const upstream = await listen(async (req, res) => {
    for await (const c of req) received.push(c);
    res.writeHead(413, { 'content-type': 'text/plain' }).end('upstream limit');
  });
  const broker = credentialBroker();
  broker.register('PROVIDER_big', upstream, 'real');
  const front = await listen((req, res) => void broker.route(req, res).then((handled) => handled || res.writeHead(404).end()));
  const body = Buffer.alloc(3 << 20, 'abc');
  const res = await fetch(`${front}/proxy/PROVIDER_big/v1/chat`, { method: 'POST', headers: { authorization: `Bearer ${broker.mint('a')}`, 'content-type': 'application/octet-stream' }, body });
  assert.deepEqual([res.status, await res.text()], [413, 'upstream limit']);
  assert.ok(Buffer.concat(received).equals(body));
});

test('a stopped session loses its token', async () => {
  const { driver, broker } = await setup();
  const minted: string[] = [];
  const mint = broker.mint;
  broker.mint = (key) => (minted.push(mint(key)), minted.at(-1)!);
  await driver.ensure('a');
  const front = await listen((req, res) => void broker.route(req, res).then(() => res.writableEnded || res.end()));
  broker.register('QUACK_X', front, 't');
  const status = async () => (await fetch(`${front}/proxy/QUACK_X/`, { headers: { authorization: `Bearer ${minted[0]}` } })).status;
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
  await mkdir(join(dir, 'abc'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  const launch = dockerLauncher({ api: dead, broker, image: 'i', runtime: 'runsc', network: 'n', data: { hostDir: '/h' }, distributorUrl: 'http://d', inherit: () => ({}), urlFor: () => 'http://127.0.0.1:9', sleep: async () => {}, startTimeoutMs: 1000 });
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
  assert.throws(() => parseHostConfig({ QUACK_SESSION_DRIVER: 'docker' }), /QUACK_DATA_VOLUME/);
  const ok = { QUACK_SESSION_DRIVER: 'docker', QUACK_HOST_DATA_DIR: '/srv/d', QUACK_SESSION_DISTRIBUTOR_URL: 'http://172.30.0.2:8080/', QUACK_SESSION_EGRESS_PROXY: 'http://172.30.0.3:3128', QUACK_SESSION_IMAGE: 'quack:1' };
  assert.deepEqual(parseHostConfig(ok), { driver: 'docker', runtime: 'runsc', data: { hostDir: '/srv/d' }, distributorUrl: 'http://172.30.0.2:8080', egressProxy: 'http://172.30.0.3:3128', image: 'quack:1', network: 'quack-sessions', dockerHost: '/var/run/docker.sock', passthrough: [] });
  assert.throws(() => parseHostConfig({ ...ok, QUACK_HOST_DATA_DIR: 'data' }), /absolute/);
  assert.throws(() => parseHostConfig({ ...ok, QUACK_DATA_VOLUME: 'v' }), /not both/);
  const { QUACK_HOST_DATA_DIR: _, ...noDir } = ok;
  assert.deepEqual((parseHostConfig({ ...noDir, QUACK_DATA_VOLUME: 'quack_quack-v1-data' }) as { data: unknown }).data, { volume: 'quack_quack-v1-data' });
  assert.throws(() => parseHostConfig({ ...ok, QUACK_SESSION_RUNTIME: 'runc' }), /QUACK_ALLOW_RUNC/);
  assert.equal((parseHostConfig({ ...ok, QUACK_SESSION_RUNTIME: 'runc', QUACK_ALLOW_RUNC: '1' }) as { runtime: string }).runtime, 'runc');
});

test('session containers name the distributor and the egress proxy by address, since they have no DNS', () => {
  const ok = { QUACK_SESSION_DRIVER: 'docker', QUACK_HOST_DATA_DIR: '/srv/d', QUACK_SESSION_DISTRIBUTOR_URL: 'http://172.30.0.2:8081', QUACK_SESSION_EGRESS_PROXY: 'http://172.30.0.3:3128', QUACK_SESSION_IMAGE: 'quack:1' };
  assert.throws(() => parseHostConfig({ ...ok, QUACK_SESSION_DISTRIBUTOR_URL: 'http://quack-v1:8081' }), /QUACK_SESSION_DISTRIBUTOR_URL must name its host by IP address/);
  assert.throws(() => parseHostConfig({ ...ok, QUACK_SESSION_EGRESS_PROXY: 'http://egress:3128' }), /QUACK_SESSION_EGRESS_PROXY must name its host by IP address/);
  const { QUACK_SESSION_EGRESS_PROXY: _, ...noProxy } = ok;
  assert.throws(() => parseHostConfig(noProxy), /QUACK_SESSION_EGRESS_PROXY is required/);
  assert.equal((parseHostConfig({ ...ok, QUACK_SESSION_DISTRIBUTOR_URL: 'http://[fd00:30::2]:8081' }) as { distributorUrl: string }).distributorUrl, 'http://[fd00:30::2]:8081');
});

test('a session reaches everything but the distributor through the egress proxy, Node included', async () => {
  const broker = credentialBroker();
  const { env } = containerEnv({}, { key: 'k', token: 't', broker, distributorUrl: 'http://172.30.0.2:8081', egressProxy: 'http://172.30.0.3:3128', passthrough: [], dir: '/data/x' });
  for (const name of ['HTTP_PROXY', 'HTTPS_PROXY', 'http_proxy', 'https_proxy']) assert.equal(env[name], 'http://172.30.0.3:3128', name);
  assert.equal(env.NODE_USE_ENV_PROXY, '1');
  for (const name of ['NO_PROXY', 'no_proxy']) assert.deepEqual(env[name].split(','), ['172.30.0.2', 'localhost', '127.0.0.1'], name);
  const bare = containerEnv({ HTTPS_PROXY: 'http://host-proxy:1' }, { key: 'k', token: 't', broker, distributorUrl: 'http://d', passthrough: [], dir: '/data/x' }).env;
  assert.ok(!Object.keys(bare).some((n) => /proxy/i.test(n)), 'the distributor\'s own proxy settings never reach a session');
  const { driver, docker } = await setup({}, undefined, 'http://172.30.0.3:3128');
  await driver.ensure('a');
  assert.ok([...docker.containers.values()][0].body.Env.includes('HTTPS_PROXY=http://172.30.0.3:3128'), 'the launcher passes its proxy to the container');
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

test('a model response streams through the broker as it is produced', async () => {
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  const upstream = await listen(async (_req, res) => {
    res.writeHead(200, { 'content-type': 'text/event-stream' });
    res.write('data: one\n\n');
    await gate;
    res.end('data: two\n\n');
  });
  const broker = credentialBroker();
  broker.register('PROVIDER_local', upstream, {});
  const front = await listen((req, res) => void broker.route(req, res));
  const res = await fetch(`${front}/proxy/PROVIDER_local/chat/completions`, { method: 'POST', headers: { authorization: `Bearer ${broker.mint('a')}` }, body: '{}' });
  const reader = res.body!.getReader();
  assert.match(new TextDecoder().decode((await reader.read()).value), /one/, 'the first chunk arrives before the upstream finishes');
  release();
  assert.match(new TextDecoder().decode((await reader.read()).value), /two/);
});

test('a container gets its server token as a file in its own mount, listens on its interface, and is probed with that token', async () => {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-docker-'));
  cleanups.push(() => rm(dataDir, { recursive: true, force: true }));
  const probes: Array<string | undefined> = [];
  const sessionUrl = await listen((req, res) => (probes.push(req.headers.authorization), res.end('{}')));
  const docker = fakeDocker(sessionUrl);
  const launch = dockerLauncher({ api: docker.api, broker: credentialBroker(), image: 'i', runtime: 'runsc', network: 'n', data: { hostDir: '/h' }, distributorUrl: 'http://d:8081', inherit: () => ({}), urlFor: () => sessionUrl, sleep: async () => {}, startTimeoutMs: 1000 });
  const driver = sessionHost({ dataDir }, launch);
  await driver.ensure('a');
  const token = driver.credential('a')!;
  assert.equal(await readFile(join(dataDir, sessionDirName('a'), '.launch-token'), 'utf8'), token);
  const env: string[] = [...docker.containers.values()][0].body.Env;
  assert.ok(env.includes('QUACK_SESSION_TOKEN_FILE=/session/.launch-token') && env.includes('QUACK_SESSION_HOST=0.0.0.0'));
  assert.ok(!env.some((e) => e.includes(token)), 'the token is never in the environment, which its shells could read');
  assert.deepEqual(probes, [`Bearer ${token}`]);
});

const fenced = { Internal: true, EnableIPv6: true, Options: { 'com.docker.network.bridge.gateway_mode_ipv4': 'isolated', 'com.docker.network.bridge.gateway_mode_ipv6': 'isolated' }, IPAM: { Config: [{ Subnet: '172.30.0.0/16' }, { Subnet: 'fd00:30::/64' }] } };
const sessionNet: DockerCall = async (_m, path) => {
  if (path === '/networks/quack-sessions') return { status: 200, body: fenced };
  return path.startsWith('/containers/json') ? { status: 200, body: [] } : { status: 404, body: { message: 'no such network' } };
};
// This host as a container on the proxy network (eth0) and the session network (eth1).
const nic = (address: string) => ({ address, family: address.includes(':') ? 'IPv6' : 'IPv4', internal: address === '127.0.0.1' }) as NetworkInterfaceInfo;
const attached = { lo: [nic('127.0.0.1')], eth0: [nic('172.18.0.3')], eth1: [nic('172.30.0.7'), nic('fe80::1')] };

test('the public listener can tell peers on the session network from everyone else, and a network it cannot read stops the boot', async () => {
  const { refuse } = await onSessionNetwork(sessionNet, 'quack-sessions', attached);
  assert.deepEqual(['172.30.4.2', '::ffff:172.30.4.2', 'fd00:30::9', '172.31.0.2', '127.0.0.1', '::1', undefined].map((a) => refuse(a)), [true, true, true, false, false, false, false]);
  await assert.rejects(onSessionNetwork(sessionNet, 'gone', attached), /no subnet/);
});

test('sessions do not boot on a network with a route out or a host address on its bridge', async () => {
  const on = (net: object): DockerCall => async () => ({ status: 200, body: net });
  const v4Only = { 'com.docker.network.bridge.gateway_mode_ipv4': 'isolated' };
  for (const net of [{ ...fenced, Internal: false }, { ...fenced, Options: {} }, { ...fenced, Options: v4Only }]) {
    await assert.rejects(onSessionNetwork(on(net), 'quack-sessions', attached), /must be internal with an isolated gateway/, JSON.stringify(net));
  }
  assert.equal((await onSessionNetwork(on({ ...fenced, EnableIPv6: false, Options: v4Only }), 'quack-sessions', attached)).address, '172.30.0.7', 'an IPv4-only network needs only its IPv4 gateway isolated');
});

test('the session listener binds loopback under the process driver and only the session-network address under docker, or the boot stops', async () => {
  const o = { dataDir: '/srv/quack', runtime: 'runtime.ts', sessionPort: 8081, broker: credentialBroker() };
  assert.equal((await sessionDriver({}, o)).listenAddress, '127.0.0.1');
  const docker = { QUACK_SESSION_DRIVER: 'docker', QUACK_HOST_DATA_DIR: '/srv/d', QUACK_SESSION_DISTRIBUTOR_URL: 'http://172.30.0.7:8081', QUACK_SESSION_EGRESS_PROXY: 'http://172.30.0.3:3128', QUACK_SESSION_IMAGE: 'quack:1' };
  assert.equal((await sessionDriver(docker, o, { api: sessionNet, interfaces: attached })).listenAddress, '172.30.0.7');
  const detached = { lo: attached.lo, eth0: attached.eth0 };
  await assert.rejects(sessionDriver(docker, o, { api: sessionNet, interfaces: detached }), /no address on docker network quack-sessions/);
});
