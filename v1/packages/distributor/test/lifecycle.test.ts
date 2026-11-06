import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readdir, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createDistributor, sessionDirName, sessionHost, type Launch } from '../src/index.ts';

const cleanups: Array<() => unknown> = [];
after(async () => {
  for (const fn of cleanups) await fn();
});

// Yields to pending I/O and promises until `probe` holds; bounded in turns, not time, so a broken path fails instead of spinning.
async function settles(probe: () => boolean) {
  for (let i = 0; i < 10_000 && !probe(); i++) await new Promise((r) => setImmediate(r));
  assert.ok(probe(), 'the state was never reached');
}

// A host whose launches are records: each start waits for `release`, so a test decides when a process comes up.
async function host(agents: string[] = []) {
  const root = await mkdtemp(join(tmpdir(), 'quack-life-'));
  cleanups.push(() => rm(root, { recursive: true, force: true }));
  const agentsDir = join(root, 'agents');
  for (const name of agents) {
    await mkdir(join(agentsDir, name), { recursive: true });
    await writeFile(join(agentsDir, name, 'agent.json'), JSON.stringify({ name, version: 1, prompt: 'prompt.md' }));
    await writeFile(join(agentsDir, name, 'prompt.md'), `${name} prompt`);
  }
  const launches: Array<{ dir: string; stopped: boolean; release: () => void }> = [];
  let held = false;
  const launch: Launch = async ({ dir }) => {
    const entry = { dir, stopped: false, release: () => {} };
    launches.push(entry);
    if (held) await new Promise<void>((r) => (entry.release = r));
    return { url: `http://launch-${launches.length}`, stop: async () => void (entry.stopped = true), onExit: () => {} };
  };
  const driver = sessionHost({ dataDir: join(root, 'data'), agentsDir, defaultAgent: '' }, launch);
  return { driver, launches, hold: () => void (held = true), dataDir: join(root, 'data'), agentsDir };
}

test('a delete during a start waits for it, stops what came up, and a later session starts fresh', async () => {
  const { driver, launches, hold, dataDir } = await host();
  hold();
  const first = driver.ensure('research:x');
  await settles(() => launches.length > 0);
  const removed = driver.remove('research:x');
  launches[0].release();
  assert.equal(await first, 'http://launch-1');
  assert.equal(await removed, true);
  assert.equal(launches[0].stopped, true, 'the process that came up mid-delete is stopped');
  assert.deepEqual(await readdir(dataDir).catch(() => []), []);
  const again = driver.ensure('research:x');
  await settles(() => launches.length >= 2);
  launches[1].release();
  assert.equal(await again, 'http://launch-2', 'the new session gets its own process, not the deleted one');
});

test('a stop during a start waits for it and stops what came up', async () => {
  const { driver, launches, hold } = await host();
  hold();
  const started = driver.ensure('research:y');
  await settles(() => launches.length > 0);
  const stopped = driver.stop('research:y');
  launches[0].release();
  await Promise.all([started, stopped]);
  assert.equal(launches[0].stopped, true);
  assert.deepEqual(driver.running(), []);
});

test('two first requests for one new key bind it once: the second agent is refused, not copied over the first', async () => {
  const { driver, launches } = await host(['reviewer', 'implementer']);
  const [a, b] = await Promise.allSettled([driver.ensure('github:o/r#1', 'reviewer'), driver.ensure('github:o/r#1', 'implementer')]);
  assert.equal(a.status, 'fulfilled');
  assert.match(String((b as PromiseRejectedResult).reason), /runs agent reviewer, not implementer/);
  assert.equal(await driver.agentOf('github:o/r#1'), 'reviewer');
  assert.equal(launches.length, 1);
  const same = await Promise.all(Array.from({ length: 5 }, () => driver.ensure('github:o/r#2', 'reviewer')));
  assert.equal(new Set(same).size, 1, 'concurrent first requests share one process');
  assert.deepEqual((await readdir(join(launches[1].dir, 'agent-def'))).sort(), ['agent.json', 'prompt.md']);
});

test('a delete is refused while a request for the session is starting it, and nothing is left behind', async () => {
  const { driver, launches, hold } = await host();
  await driver.ensure('research:z');
  await driver.stop('research:z');
  hold();
  const upstream = createServer((_req, res) => res.end('{}')).listen(0);
  await new Promise((r) => upstream.once('listening', r));
  cleanups.push(() => upstream.close());
  const url = `http://127.0.0.1:${(upstream.address() as AddressInfo).port}`;
  const server = createDistributor({ ...driver, ensure: async (k, a) => (await driver.ensure(k, a), url) }, { retire: { token: 't', waiting: async () => false, forget: async () => {} } }).listen(0);
  await new Promise((r) => server.once('listening', r));
  cleanups.push(() => server.close());
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/research%3Az`;
  const read = fetch(`${base}/messages`);
  await settles(() => launches.length >= 2);
  const del = await fetch(base, { method: 'DELETE', headers: { authorization: 'Bearer t' } });
  assert.equal(del.status, 409);
  assert.match((await del.json()).detail, /handling a request/);
  launches[1].release();
  assert.equal((await read).status, 200);
  assert.equal((await fetch(base, { method: 'DELETE', headers: { authorization: 'Bearer t' } })).status, 200);
  assert.equal(launches[1].stopped, true);
});

test('a new session checks its agent version against the hash recorded at first use, not every session on disk', async () => {
  const { driver, dataDir, agentsDir } = await host(['reviewer']);
  await driver.ensure('github:a#1', 'reviewer');
  assert.equal(await driver.remove('github:a#1'), true);
  await writeFile(join(agentsDir, 'reviewer', 'prompt.md'), 'edited without a version bump');
  await assert.rejects(driver.ensure('github:a#2', 'reviewer'), /changed since it was first used/, 'the record outlives the sessions that pinned it');
  assert.deepEqual(await readdir(dataDir), ['agent-versions']);
});

test('a version pinned before hashes were recorded is checked once against its snapshots, then recorded', async () => {
  const { driver, dataDir, agentsDir } = await host(['reviewer']);
  const old = join(dataDir, sessionDirName('github:old'));
  await mkdir(join(old, 'agent-def'), { recursive: true });
  await writeFile(join(old, 'session-key'), 'github:old');
  await writeFile(join(old, 'agent'), 'reviewer');
  await writeFile(join(old, 'agent-def', 'agent.json'), JSON.stringify({ name: 'reviewer', version: 1, prompt: 'prompt.md' }));
  await writeFile(join(old, 'agent-def', 'prompt.md'), 'the prompt it first ran');
  await assert.rejects(driver.ensure('github:new', 'reviewer'), /changed since it was first used/);
  await writeFile(join(agentsDir, 'reviewer', 'prompt.md'), 'the prompt it first ran');
  await rm(old, { recursive: true });
  assert.equal(await driver.ensure('github:new', 'reviewer'), 'http://launch-1', 'the recorded hash is the snapshot it first ran');
});

test('one session reads from its own directory, the same row the listing shows', async () => {
  const { driver } = await host(['reviewer']);
  await driver.ensure('github:a#1', 'reviewer');
  await driver.ensure('github:a#2', 'reviewer');
  const [listed] = (await driver.sessions()).filter((s) => s.key === 'github:a#1');
  assert.deepEqual(await driver.session('github:a#1'), listed);
  assert.equal(await driver.session('github:none'), undefined);
});
