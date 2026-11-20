import assert from 'node:assert/strict';
import { openCatalog } from '@quack/catalog';
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { CURRENT_AGENT_FILE, withCurrentExtensions, type Catalog } from '@quack/session';
import { createDistributor, sessionDirName, sessionHost, type Launch } from '../src/index.ts';

const cleanups: Array<() => unknown> = [];
after(async () => {
  for (const fn of cleanups) await fn();
});

// A host whose launches are records: each start waits for `release`, so a test decides when a process comes up.
async function host(agents: string[] = []) {
  const root = await mkdtemp(join(tmpdir(), 'quack-life-'));
  cleanups.push(() => rm(root, { recursive: true, force: true }));
  const agentsDir = join(root, 'agents');
  for (const name of agents) {
    await mkdir(join(agentsDir, name), { recursive: true });
    await writeFile(join(agentsDir, name, 'agent.json'), JSON.stringify({ name, version: 1, prompt: 'prompt.md', extensions: {} }));
    await writeFile(join(agentsDir, name, 'prompt.md'), `${name} prompt`);
  }
  const launches: Array<{ dir: string; stopped: boolean; release: () => void }> = [];
  let held = false;
  const waiters: Array<[number, () => void]> = [];
  const launch: Launch = async ({ dir }) => {
    const entry = { dir, stopped: false, release: () => {} };
    launches.push(entry);
    for (const [n, wake] of waiters) if (launches.length >= n) wake();
    if (held) await new Promise<void>((r) => (entry.release = r));
    return { url: `http://launch-${launches.length}`, token: 't', stop: async () => void (entry.stopped = true), onExit: () => {} };
  };
  const driver = sessionHost({ dataDir: join(root, 'data'), agentsDir, defaultAgent: '' }, launch);
  // Resolves when the nth launch has begun; the driver does real file I/O first, so no turn count can bound that wait.
  const launched = (n: number) => (launches.length >= n ? Promise.resolve() : new Promise<void>((r) => waiters.push([n, r])));
  return { driver, launches, launched, hold: () => void (held = true), dataDir: join(root, 'data'), agentsDir };
}

test('a delete during a start waits for it, stops what came up, and a later session starts fresh', async () => {
  const { driver, launches, launched, hold, dataDir } = await host();
  hold();
  const first = driver.ensure('research:x');
  await launched(1);
  const removed = driver.remove('research:x');
  launches[0].release();
  assert.equal(await first, 'http://launch-1');
  assert.equal(await removed, true);
  assert.equal(launches[0].stopped, true, 'the process that came up mid-delete is stopped');
  assert.deepEqual(await readdir(dataDir).catch(() => []), []);
  const again = driver.ensure('research:x');
  await launched(2);
  launches[1].release();
  assert.equal(await again, 'http://launch-2', 'the new session gets its own process, not the deleted one');
});

test('a stop during a start waits for it and stops what came up', async () => {
  const { driver, launches, launched, hold } = await host();
  hold();
  const started = driver.ensure('research:y');
  await launched(1);
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
  const { driver, launches, launched, hold } = await host();
  await driver.ensure('research:z');
  await driver.stop('research:z');
  hold();
  const upstream = createServer((_req, res) => res.end('{}')).listen(0);
  await new Promise((r) => upstream.once('listening', r));
  cleanups.push(() => upstream.close());
  const url = `http://127.0.0.1:${(upstream.address() as AddressInfo).port}`;
  const catalog = openCatalog({ dataDir: await mkdtemp(join(tmpdir(), 'quack-life-catalog-')) });
  cleanups.push(() => void catalog.close());
  await catalog.publish('research:z', [{ seq: 1, id: 'session:z', at: 0, type: 'session.created', data: { agent: 'a', createdAt: 0 } }]);
  const server = createDistributor({ ...driver, ensure: async (k, a) => (await driver.ensure(k, a), url) }, { catalog }).listen(0);
  await new Promise((r) => server.once('listening', r));
  cleanups.push(() => server.close());
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}/sessions/research%3Az`;
  const read = fetch(`${base}/messages`);
  await launched(2);
  const del = await fetch(base, { method: 'DELETE' });
  assert.equal(del.status, 409);
  assert.match((await del.json()).detail, /handling a request/);
  launches[1].release();
  assert.equal((await read).status, 200);
  assert.equal((await fetch(base, { method: 'DELETE' })).status, 200);
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


test('a woken session keeps its pinned bundle but judges with its agent\'s current one, and fails clearly once that bundle is removed', async () => {
  const { driver, launches, agentsDir } = await host();
  const bundle = async (version: number, judge: object) => {
    await mkdir(join(agentsDir, 'contracts'), { recursive: true });
    await writeFile(join(agentsDir, 'contracts', 'agent.json'), JSON.stringify({ name: 'contracts', version, prompt: 'prompt.md', extensions: { judge, contracts: { v: version } } }));
    await writeFile(join(agentsDir, 'contracts', 'prompt.md'), `prompt v${version}`);
  };
  const clef = { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 };
  await bundle(6, { maxRevisions: 2 });
  await driver.ensure('contracts:a', 'contracts');
  await driver.stop('contracts:a');
  await bundle(13, { maxRevisions: 2, decisionModel: clef });
  await driver.ensure('contracts:a');
  const dir = launches[1].dir;
  const pinned = JSON.parse(await readFile(join(dir, 'agent-def', 'agent.json'), 'utf8'));
  const liveCatalog: Catalog = { contracts: () => [], judge: Object.assign(() => [], { configSource: 'current' as const }) };
  const runs = withCurrentExtensions(pinned, JSON.parse(await readFile(join(dir, CURRENT_AGENT_FILE), 'utf8')), liveCatalog);
  assert.deepEqual(runs.extensions, { contracts: { v: 6 }, judge: { maxRevisions: 2, decisionModel: clef } }, 'the judge follows v13, everything else stays on v6');
  assert.equal(await readFile(join(dir, 'agent-def', 'prompt.md'), 'utf8'), 'prompt v6');

  await driver.stop('contracts:a');
  await rm(join(agentsDir, 'contracts'), { recursive: true });
  await assert.rejects(driver.ensure('contracts:a'), /runs agent contracts, which is no longer installed/);
  assert.equal(launches.length, 2, 'nothing starts without the current bundle');
});
