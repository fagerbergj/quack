import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import type { ExtensionManifest } from '@quack/sdk';
import { createDistributor, processDriver, sessionDirName } from '../src/index.ts';
import { rubricFrom } from '../src/rubrics.ts';

const cleanups: Array<() => void> = [];
after(() => cleanups.forEach((fn) => fn()));

const write = async (dir: string, def: object, prompt: string) => {
  await mkdir(dir, { recursive: true });
  await writeFile(join(dir, 'agent.json'), JSON.stringify(def));
  await writeFile(join(dir, 'prompt.md'), prompt);
};
const bundle = (version: number) => ({ name: 'reviewer', version, model: 'local/m', description: 'Reviews PRs.', prompt: 'prompt.md', extensions: { github: {} } });
const rubric = { criteria: [{ id: 'c', checks: [{ id: 'k', label: 'Names a file', weight: 'gate' }] }] };

const tmp = async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-agents-'));
  cleanups.push(() => void rm(dir, { recursive: true, force: true }));
  return dir;
};

// A distributor over `dataDir`, serving whatever `agentsDir` holds right now.
async function start(dataDir: string, agentsDir: string) {
  const driver = processDriver({ command: [], dataDir, agentsDir, rubricFor: (names) => (names.includes('github') ? rubric : undefined) });
  const server = createDistributor(driver, {}).listen(0);
  await new Promise((r) => server.once('listening', r));
  cleanups.push(() => server.close());
  const url = (path: string) => `http://127.0.0.1:${(server.address() as AddressInfo).port}${path}`;
  const get = async (path: string) => (await (await fetch(url(path))).json()) as Record<string, any>;
  const post = (body: unknown, headers: Record<string, string> = { 'content-type': 'application/json' }) => fetch(url('/agents/reviewer/promote'), { method: 'POST', headers, body: JSON.stringify(body) });
  return { get, post, url };
}

test('requests racing the boot copy all see the stored version', async () => {
  const [dataDir, agentsDir] = [await tmp(), join(await tmp(), 'agents')];
  await write(join(agentsDir, 'reviewer'), bundle(3), 'new prompt');
  const { get } = await start(dataDir, agentsDir);
  const seen = await Promise.all([get('/agents'), ...Array.from({ length: 8 }, () => get('/agents/reviewer'))]);
  for (const r of seen.slice(1)) assert.deepEqual([r.version, r.prompt, r.rubric], [3, 'new prompt', rubric]);
});

test('an agent reads at its current version with its rubric, or one a session pinned', async () => {
  const [dataDir, agentsDir] = [await tmp(), join(await tmp(), 'agents')];
  await write(join(agentsDir, 'reviewer'), bundle(3), 'new prompt');
  await write(join(dataDir, sessionDirName('old'), 'agent-def'), { name: 'reviewer', version: 2, status: 'promoted', prompt: 'prompt.md' }, 'old prompt');
  const { get, url } = await start(dataDir, agentsDir);
  assert.deepEqual((await get('/agents')).agents, [{ name: 'reviewer', version: 3, status: 'draft', model: 'local/m', description: 'Reviews PRs.' }]);
  const now = await get('/agents/reviewer');
  assert.deepEqual([now.version, now.prompt, now.rubric, now.versions], [3, 'new prompt', rubric, [{ version: 3, status: 'draft' }, { version: 2, status: 'promoted' }]]);
  const old = await get('/agents/reviewer?version=2');
  assert.deepEqual([old.version, old.prompt, old.rubric], [2, 'old prompt', undefined]);
  assert.equal((await fetch(url('/agents/reviewer?version=9'))).status, 404);
  assert.equal((await fetch(url('/agents/..%2Fx'))).status, 404);
});

test('a version is kept when first loaded and stays readable after the agents dir moves on', async () => {
  const [dataDir, root] = [await tmp(), await tmp()];
  await write(join(root, 'a', 'reviewer'), bundle(3), 'prompt three');
  await (await start(dataDir, join(root, 'a'))).get('/agents');
  await write(join(root, 'b', 'reviewer'), bundle(4), 'prompt four');
  const { get } = await start(dataDir, join(root, 'b'));
  const v3 = await get('/agents/reviewer?version=3');
  assert.deepEqual([v3.prompt, v3.rubric, (await get('/agents/reviewer')).versions.map((v: { version: number }) => v.version)], ['prompt three', rubric, [4, 3]]);
});

test('promotion survives a restart on a fresh agents dir, and the agents dir is untouched', async () => {
  const [dataDir, root] = [await tmp(), await tmp()];
  await write(join(root, 'a', 'reviewer'), bundle(3), 'p');
  const first = await start(dataDir, join(root, 'a'));
  assert.equal((await first.post({ version: 3 }, {})).status, 415);
  assert.equal((await first.post({ version: 2 })).status, 409);
  assert.equal((await first.post({})).status, 400);
  assert.equal((await first.post({ version: 3 })).status, 200);
  assert.equal(JSON.parse(await readFile(join(root, 'a', 'reviewer', 'agent.json'), 'utf8')).status, undefined);
  await write(join(root, 'b', 'reviewer'), bundle(3), 'p');
  const second = await start(dataDir, join(root, 'b'));
  assert.equal((await second.get('/agents')).agents[0].status, 'promoted');
  assert.equal((await fetch(second.url('/agents/nope/promote'), { method: 'POST', headers: { 'content-type': 'application/json' }, body: '{"version":1}' })).status, 404);
});

test('an agent gets the rubric of the first selected extension whose eval adapter judges', () => {
  const manifest = (name: string, rubric?: unknown): ExtensionManifest => ({ name, evalAdapters: [{ kind: 'k', judge: rubric ? { rubric, evidence: async () => undefined } : undefined }] });
  const pick = rubricFrom([manifest('github', rubric), manifest('research')]);
  assert.deepEqual([pick(['coding', 'github']), pick(['research']), pick([])], [rubric, undefined, undefined]);
});
