import assert from 'node:assert/strict';
import { createSocket } from 'node:dgram';
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { credentialBroker, dockerApi, dockerLauncher, parseDockerHost, sessionDirName, sessionHost, type DockerCall } from '../src/index.ts';

// Opt-in: QUACK_LIVE_IMAGE=<quack image> QUACK_LIVE_RUNTIME=runsc, with the Docker daemon on /var/run/docker.sock
// (or QUACK_DOCKER_HOST) and the temp dir visible to the daemon at the same path. The v1 isolation job runs it.
const image = process.env.QUACK_LIVE_IMAGE;
const runtime = process.env.QUACK_LIVE_RUNTIME;

// A stand-in session: answers /ping, and on /probe reports what it can see of the host.
const probe = `
const fs = require('fs');
const environs = fs.readdirSync('/proc').filter((p) => /^\\d+$/.test(p)).map((p) => { try { return fs.readFileSync('/proc/' + p + '/environ', 'utf8'); } catch { return ''; } }).join('');
require('http').createServer((req, res) => {
  const sessions = fs.readdirSync('/session').filter((f) => f !== '.');
  res.end(JSON.stringify({ sawSecret: environs.includes('QUACK_LIVE_SECRET'), sessions, token: process.env.QUACK_GH_SERVICE_TOKEN }));
}).listen(8080);`;

test('a live session container cannot see the distributor environment or other sessions', { skip: !image || !runtime }, async () => {
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-live-'));
  process.env.QUACK_LIVE_SECRET = 'distributor-only';
  try {
    await mkdir(join(dataDir, sessionDirName('other')), { recursive: true });
    await writeFile(join(dataDir, sessionDirName('other'), 'private'), 'other session');
    const api = dockerApi(parseDockerHost(process.env.QUACK_DOCKER_HOST ?? '/var/run/docker.sock'));
    const launch = dockerLauncher({
      api, broker: credentialBroker(), image: image!, runtime: runtime!, network: process.env.QUACK_LIVE_NETWORK ?? 'bridge', data: { hostDir: dataDir }, distributorUrl: 'http://distributor:8080',
      inherit: () => ({ QUACK_GH_SERVICE_URL: 'http://gh', QUACK_GH_SERVICE_TOKEN: 'long-lived' }), command: ['node', '-e', probe], urlFor: () => process.env.QUACK_LIVE_URL ?? 'http://127.0.0.1:8080', startTimeoutMs: 120_000,
    });
    const driver = sessionHost({ dataDir }, launch);
    const url = await driver.ensure('a');
    const seen = (await (await fetch(`${url}/probe`)).json()) as { sawSecret: boolean; sessions: string[]; token: string };
    assert.equal(seen.sawSecret, false);
    assert.ok(!seen.sessions.includes('private') && seen.sessions.every((f) => f !== sessionDirName('other')));
    assert.notEqual(seen.token, 'long-lived');
    await driver.stop('a');
  } finally {
    delete process.env.QUACK_LIVE_SECRET;
    await rm(dataDir, { recursive: true, force: true });
  }
});

// Keep in step with the egress proxy in docs/README.md.
const PROXY_IMAGE = 'ubuntu/squid:6.6-24.04_edge@sha256:8a3baed477e2c282ab8aa5edad442f69873246964f225c5c2ae8364b6610963c';
const SQUID_CONF = fileURLToPath(new URL('../egress/squid.conf', import.meta.url));

// Waits for /session/go so it cannot exit before the launch has finished, then reports what it reached directly and through the proxy.
const egressProbe = `
const fs = require('fs'), net = require('net'), { execFileSync } = require('child_process');
const dial = (host, port) => new Promise((ok) => { const s = net.connect({ host, port, timeout: 3000 }); s.on('connect', () => (s.destroy(), ok(true))).on('error', () => ok(false)).on('timeout', () => (s.destroy(), ok(false))); });
const status = (url) => fetch(url, { signal: AbortSignal.timeout(15000) }).then((r) => r.status, (e) => String(e.cause?.message ?? e.message));
const pause = (ms) => new Promise((r) => setTimeout(r, ms));
const clone = () => { try { execFileSync('git', ['ls-remote', 'https://github.com/fagerbergj/quack', 'HEAD'], { stdio: 'ignore', timeout: 20000 }); return true; } catch { return false; } };
(async () => {
  while (!fs.existsSync('/session/go')) await pause(100);
  const [ip, port, published, gateway] = process.env.QUACK_LIVE_TARGETS.split(',');
  let github = false;
  for (let i = 0; i < 20 && !github; i++) if (!(github = clone())) await pause(1000);
  const direct = { internet: await dial('1.1.1.1', 443), host: await dial(ip, +port), gateway: await dial(gateway, +port), published: await dial(ip, +published) };
  const proxied = { host: await status('http://' + ip + ':' + port + '/'), tailnet: await status('http://100.64.0.6:18081/'), github, fetch: await status('https://api.github.com/zen') };
  fs.writeFileSync('/session/report.json', JSON.stringify({ direct, proxied }));
})();`;

// The source address of this host's default route; connecting a UDP socket sends nothing.
const hostAddress = () => new Promise<string>((resolve, reject) => {
  const s = createSocket('udp4');
  s.on('error', reject).connect(53, '1.1.1.1', () => (resolve(s.address().address), s.close()));
});

async function must(api: DockerCall, method: 'GET' | 'POST' | 'DELETE', path: string, body?: unknown) {
  const res = await api(method, path, body);
  assert.ok(res.status < 300, `${method} ${path} answered ${res.status}: ${JSON.stringify(res.body)}`);
  return res.body as any;
}

test('a live session reaches public hosts only through the egress proxy, and never a private address or the host', { skip: !image || !runtime }, async () => {
  const api = dockerApi(parseDockerHost(process.env.QUACK_DOCKER_HOST ?? '/var/run/docker.sock'));
  const tag = `quack-live-${process.pid}`;
  const dataDir = await mkdtemp(join(tmpdir(), 'quack-live-'));
  const host = createServer((_req, res) => res.end('host')).listen(0, '0.0.0.0');
  await new Promise((r) => host.once('listening', r));
  try {
    await must(api, 'POST', `/images/create?fromImage=${encodeURIComponent(PROXY_IMAGE)}`);
    const sessions = (await must(api, 'POST', '/networks/create', { Name: `${tag}-sessions`, Internal: true, Options: { 'com.docker.network.bridge.gateway_mode_ipv4': 'isolated' } })).Id as string;
    const egress = (await must(api, 'POST', '/networks/create', { Name: `${tag}-egress` })).Id as string;
    const subnet: string = (await must(api, 'GET', `/networks/${sessions}`)).IPAM.Config[0].Subnet;
    const [proxyIp, gateway] = ['250', '1'].map((n) => subnet.split('/')[0]!.replace(/\d+$/, n));
    const ip = await hostAddress();
    await must(api, 'POST', `/containers/create?name=${tag}-proxy`, {
      Image: PROXY_IMAGE, Cmd: ['-NYC'],
      HostConfig: { NetworkMode: egress, Binds: [`${SQUID_CONF}:/etc/squid/squid.conf:ro`], PortBindings: { '3128/tcp': [{ HostIp: ip, HostPort: '' }] } },
      NetworkingConfig: { EndpointsConfig: { [egress]: {}, [sessions]: { IPAMConfig: { IPv4Address: proxyIp } } } },
    });
    await must(api, 'POST', `/containers/${tag}-proxy/start`);
    const published = (await must(api, 'GET', `/containers/${tag}-proxy/json`)).NetworkSettings.Ports['3128/tcp'][0].HostPort as string;
    const dir = join(dataDir, sessionDirName('net'));
    await mkdir(dir);
    await chmod(dir, 0o777);
    const launch = dockerLauncher({
      api, broker: credentialBroker(), image: image!, runtime: runtime!, network: sessions, egressProxy: `http://${proxyIp}:3128`, data: { hostDir: dataDir }, distributorUrl: 'http://192.0.2.1:8081',
      inherit: () => ({ QUACK_LIVE_TARGETS: `${ip},${(host.address() as AddressInfo).port},${published},${gateway}` }), passthrough: ['QUACK_LIVE_TARGETS'],
      command: ['node', '-e', egressProbe], urlFor: () => `http://127.0.0.1:${(host.address() as AddressInfo).port}`, startTimeoutMs: 120_000,
    });
    const launched = await launch({ key: 'net', dir, agentPath: undefined });
    const exited = new Promise<void>((r) => launched.onExit(r));
    await writeFile(join(dir, 'go'), '');
    await exited;
    const { direct, proxied } = JSON.parse(await readFile(join(dir, 'report.json'), 'utf8'));
    assert.deepEqual(direct, { internet: false, host: false, gateway: false, published: false }, 'nothing is reachable around the proxy, the bridge included');
    assert.deepEqual(proxied, { host: 403, tailnet: 403, github: true, fetch: 200 }, 'the proxy refuses private addresses and passes public hosts, for git and for Node');
  } finally {
    host.close();
    await api('DELETE', `/containers/${tag}-proxy?force=1`);
    for (const name of ['sessions', 'egress']) await api('DELETE', `/networks/${tag}-${name}`);
    await rm(dataDir, { recursive: true, force: true });
  }
});
