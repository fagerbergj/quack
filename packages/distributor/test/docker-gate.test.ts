import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { admit, credentialBroker, dockerApi, dockerGate, dockerLauncher, sessionDirName, sessionHost, type SessionContainers } from '../src/index.ts';

const cleanups: Array<() => unknown> = [];
after(async () => {
  for (const fn of cleanups) await fn();
});

const listen = async (server: Server) => {
  server.listen(0, '127.0.0.1');
  await new Promise((r) => server.once('listening', r));
  cleanups.push(() => server.close());
  return (server.address() as AddressInfo).port;
};

type Container = { Id: string; Name: string; Labels: Record<string, string>; body?: any };

// A Docker Engine over HTTP that does whatever it is asked, so every refusal in these tests is the gate's.
async function fakeDockerd() {
  const containers = new Map<string, Container>();
  const calls: string[] = [];
  const waiters = new Map<string, () => void>();
  let ids = 0;
  const server = createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c as Buffer);
    const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : undefined;
    const url = new URL(req.url!, 'http://d');
    calls.push(`${req.method} ${url.pathname}${url.search}`);
    const send = (status: number, json?: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(json === undefined ? undefined : JSON.stringify(json));
    if (url.pathname === '/containers/create') {
      const c = { Id: `id${++ids}`, Name: url.searchParams.get('name')!, Labels: body.Labels ?? {}, body };
      containers.set(c.Id, c);
      return send(201, { Id: c.Id });
    }
    if (url.pathname.startsWith('/networks/')) return send(200, { Name: 'quack-sessions' });
    if (url.pathname === '/containers/json') return send(200, [...containers.values()].map((c) => ({ Id: c.Id })));
    const [, , ref, action] = url.pathname.split('/');
    const c = [...containers.values()].find((x) => x.Id === ref || x.Name === ref);
    if (!c) return send(404, { message: 'no such container' });
    if (action === 'json') return send(200, { Id: c.Id, Config: { Labels: c.Labels } });
    if (action === 'wait') return new Promise<void>((r) => waiters.set(c.Id, r)).then(() => send(200, { StatusCode: 0 }));
    if (action !== 'start') containers.delete(c.Id), waiters.get(c.Id)?.();
    return send(204);
  });
  const port = await listen(server);
  return { containers, calls, api: dockerApi({ host: '127.0.0.1', port }) };
}

const policy: SessionContainers = { image: 'quack:pinned', runtime: 'runsc', network: 'quack-sessions', data: { volume: 'quack-data' } };
const DIR = sessionDirName('a');
const NAME = `quack-session-${DIR}`;

async function setup() {
  const dockerd = await fakeDockerd();
  const port = await listen(dockerGate(policy, dockerd.api));
  const warn = console.warn;
  console.warn = () => {};
  cleanups.push(() => (console.warn = warn));
  return { dockerd, gate: dockerApi({ host: '127.0.0.1', port }) };
}

// The create body the docker driver sends, as the first test checks; the others change one thing in it.
const good = () => ({
  Image: 'quack:pinned', Cmd: ['node', 'packages/runtime/src/main.ts'], WorkingDir: '/app', Env: ['PORT=8080'], Labels: { 'quack.session': 'a' },
  HostConfig: { Runtime: 'runsc', NetworkMode: 'quack-sessions', Mounts: [{ Type: 'volume', Source: 'quack-data', Target: '/session', VolumeOptions: { Subpath: DIR } }], AutoRemove: true, CapDrop: ['ALL'], SecurityOpt: ['no-new-privileges'] },
});

test('the docker driver runs a session through the gate: network, list, create, start, wait, stop and delete all pass', async () => {
  const { dockerd, gate } = await setup();
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-gate-'));
  cleanups.push(() => rm(dataDir, { recursive: true, force: true }));
  const sessionUrl = `http://127.0.0.1:${await listen(createServer((_req, res) => res.end('{}')))}`;
  const launch = dockerLauncher({ ...policy, api: gate, broker: credentialBroker(), distributorUrl: 'http://10.0.0.2:8081', inherit: () => ({}), urlFor: () => sessionUrl, sleep: async () => {}, startTimeoutMs: 1000 });
  assert.equal((await gate('GET', '/networks/quack-sessions')).status, 200);
  assert.equal((await gate('GET', `/containers/json?all=1&filters=${encodeURIComponent(JSON.stringify({ label: ['quack.session'] }))}`)).status, 200);
  const driver = sessionHost({ dataDir }, launch);
  await driver.ensure('a');
  const [c] = [...dockerd.containers.values()];
  const { Env, ...rest } = c!.body;
  assert.deepEqual({ ...rest, Env: ['PORT=8080'] }, good(), 'the tests below vary the body the driver really sends');
  assert.ok(Array.isArray(Env));
  await driver.stop('a');
  assert.equal(dockerd.containers.size, 0);
  for (const call of ['POST /containers/create', 'POST /containers/id1/start', 'POST /containers/id1/wait', 'POST /containers/id1/stop']) assert.ok(dockerd.calls.some((x) => x.startsWith(call)), `${call} reached dockerd`);
});

const host = (patch: Record<string, unknown>) => ({ ...good(), HostConfig: { ...good().HostConfig, ...patch } });
const refusedCreates: Array<[string, unknown, string?]> = [
  ['an extra bind', host({ Binds: ['/:/host'] })],
  ['a second mount', host({ Mounts: [...good().HostConfig.Mounts, { Type: 'bind', Source: '/', Target: '/host' }] })],
  ['another session\'s directory', host({ Mounts: [{ ...good().HostConfig.Mounts[0], VolumeOptions: { Subpath: sessionDirName('b') } }] })],
  ['another volume', host({ Mounts: [{ ...good().HostConfig.Mounts[0], Source: 'quack-secrets' }] })],
  ['Privileged', host({ Privileged: true })],
  ['runc', host({ Runtime: 'runc' })],
  ['no runtime', host({ Runtime: undefined })],
  ['host network', host({ NetworkMode: 'host' })],
  ['another image', { ...good(), Image: 'alpine' }],
  ['CapAdd', host({ CapAdd: ['SYS_ADMIN'] })],
  ['CapDrop without ALL', host({ CapDrop: ['NET_RAW'] })],
  ['Devices', host({ Devices: [{ PathOnHost: '/dev/sda', PathInContainer: '/dev/sda' }] })],
  ['the host pid namespace', host({ PidMode: 'host' })],
  ['the host ipc namespace', host({ IpcMode: 'host' })],
  ['the host user namespace', host({ UsernsMode: 'host' })],
  ['an unconfined seccomp profile', host({ SecurityOpt: ['seccomp=unconfined'] })],
  ['a second network', { ...good(), NetworkingConfig: { EndpointsConfig: { bridge: {} } } }],
  ['a case-variant key', { ...good(), image: 'alpine' }],
  ['no session label', { ...good(), Labels: {} }],
  ['an extra label', { ...good(), Labels: { 'quack.session': 'a', 'traefik.enable': 'true' } }],
  ['a name that is not a session\'s', good(), 'quack-session-other'],
  ['a name that does not match its mount', good(), `quack-session-${sessionDirName('b')}`],
  ['no body', undefined],
];

for (const [what, body, name = NAME] of refusedCreates) {
  test(`a create with ${what} is refused with 403 and never reaches dockerd`, async () => {
    const { dockerd, gate } = await setup();
    const res = await gate('POST', `/containers/create?name=${name}`, body);
    assert.equal(res.status, 403, JSON.stringify(res.body));
    assert.match((res.body as { message: string }).message, /^docker gate: /);
    assert.equal(dockerd.calls.filter((c) => c.startsWith('POST /containers/create')).length, 0);
  });
}

test('start, stop, wait and delete reach only containers with the session label, by their id', async () => {
  const { dockerd, gate } = await setup();
  dockerd.containers.set('plain', { Id: 'plain', Name: 'host-db', Labels: {} });
  for (const [method, path] of [['POST', '/containers/host-db/start'], ['POST', '/containers/host-db/stop?t=0'], ['POST', '/containers/host-db/wait'], ['DELETE', '/containers/host-db?force=1']] as const) {
    assert.equal((await gate(method, path)).status, 403, `${method} ${path}`);
  }
  assert.equal((await gate('DELETE', '/containers/missing?force=1')).status, 404, 'a missing container answers as dockerd would');
  assert.ok(dockerd.containers.has('plain'));
  assert.deepEqual(dockerd.calls.filter((c) => !c.endsWith('/json')), [], 'only lookups reached dockerd');
  assert.equal((await gate('POST', `/containers/create?name=${NAME}`, good())).status, 201);
  assert.equal((await gate('POST', `/containers/${NAME}/start`)).status, 204);
  assert.equal((await gate('DELETE', `/containers/${NAME}?force=1`)).status, 204);
  assert.ok(dockerd.calls.includes('POST /containers/id1/start') && dockerd.calls.includes('DELETE /containers/id1?force=1'));
});

test('everything else the Engine serves is refused with 403', async () => {
  const { dockerd, gate } = await setup();
  dockerd.containers.set('s', { Id: 's', Name: 's', Labels: { 'quack.session': 'x' } });
  const refused = [
    ['GET', '/containers/json?all=1'],
    ['GET', `/containers/json?filters=${encodeURIComponent(JSON.stringify({ label: ['other'] }))}`],
    ['GET', '/networks/bridge'],
    ['GET', '/containers/s/json'],
    ['GET', '/containers/s/archive?path=/'],
    ['POST', '/containers/s/exec'],
    ['POST', '/containers/s/kill'],
    ['POST', '/containers/s/start?detachKeys=x'],
    ['POST', '/networks/create'],
    ['POST', '/images/create?fromImage=alpine'],
    ['GET', '/images/json'],
    ['POST', '/v1.47/containers/create'],
    ['POST', '/containers/../images/create'],
    ['GET', '/_ping'],
  ] as const;
  for (const [method, path] of refused) assert.equal((await gate(method, path)).status, 403, `${method} ${path}`);
  assert.deepEqual(dockerd.calls.filter((c) => !c.startsWith('GET /containers/s/json')), [], 'only label lookups reached dockerd');
});

test('with a host data directory, the one bind allowed is the session\'s own directory', async () => {
  const bound = { ...policy, data: { hostDir: '/srv/quack' } };
  const create = (Binds: string[]) => admit(async () => ({ status: 404, body: undefined }), bound, 'POST', new URL(`http://gate/containers/create?name=${NAME}`), host({ Mounts: undefined, Binds }));
  assert.equal((await create([`/srv/quack/${DIR}:/session`])).path, `/containers/create?name=${NAME}`);
  await assert.rejects(create([`/srv/quack/${DIR}:/session`, '/var/run/docker.sock:/s']), /only mount allowed/);
  await assert.rejects(create(['/srv/quack:/session']), /only mount allowed/);
});
