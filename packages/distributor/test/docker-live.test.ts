import assert from 'node:assert/strict';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { credentialBroker, dockerApi, dockerLauncher, parseDockerHost, sessionDirName, sessionHost } from '../src/index.ts';

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
