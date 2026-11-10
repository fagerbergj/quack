import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { AddressInfo } from 'node:net';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createDistributor, processDriver, sessionDirName } from '../src/index.ts';

const cleanups: Array<() => Promise<void> | void> = [];
after(async () => {
  for (const fn of cleanups.reverse()) await fn();
});

// Two agents with distinct prompts, served by the given session fixture.
async function start(fixture: string) {
  const root = await mkdtemp(join(tmpdir(), 'quack-role-'));
  cleanups.push(() => rm(root, { recursive: true, force: true }));
  const agentsDir = join(root, 'agents');
  for (const name of ['reviewer', 'implementer']) {
    await mkdir(join(agentsDir, name), { recursive: true });
    await writeFile(join(agentsDir, name, 'agent.json'), JSON.stringify({ name, version: 1, prompt: 'prompt.md' }));
    await writeFile(join(agentsDir, name, 'prompt.md'), `${name} prompt`);
  }
  const dataDir = join(root, 'data');
  const driver = processDriver({ command: [process.execPath, fileURLToPath(new URL(`./fixtures/${fixture}`, import.meta.url))], dataDir, agentsDir, defaultAgent: '' });
  const server = createDistributor(driver, { idleMs: 60_000 }).listen(0);
  await new Promise((r) => server.once('listening', r));
  cleanups.push(async () => {
    await server.drain();
    await new Promise((r) => server.close(r));
  });
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const invoke = (key: string, body: object, agent?: string) =>
    fetch(`${base}/sessions/${encodeURIComponent(key)}/invocations${agent ? `?agent=${agent}` : ''}`, { method: 'POST', body: JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json() }));
  const snapshot = async (key: string) => ({
    agent: JSON.parse(await readFile(join(dataDir, sessionDirName(key), 'agent-def', 'agent.json'), 'utf8')).name as string,
    prompt: await readFile(join(dataDir, sessionDirName(key), 'agent-def', 'prompt.md'), 'utf8'),
    recorded: await readFile(join(dataDir, sessionDirName(key), 'agent'), 'utf8'),
  });
  return { driver, invoke, snapshot, base, dataDir };
}

test('a session switches agent when a run starts, keeps its log, and a restart resumes with the latest role', async () => {
  const { driver, invoke, snapshot, dataDir } = await start('echo-session.ts');
  const key = 'github:o/r#7';
  assert.equal((await invoke(key, { input: 'first', wait: true }, 'reviewer')).status, 200);
  assert.deepEqual(await snapshot(key), { agent: 'reviewer', prompt: 'reviewer prompt', recorded: 'reviewer' });

  assert.equal((await invoke(key, { input: 'second', wait: true }, 'implementer')).status, 200);
  assert.deepEqual(await snapshot(key), { agent: 'implementer', prompt: 'implementer prompt', recorded: 'implementer' });

  assert.equal((await invoke(key, { input: 'x' }, 'nope')).status, 404);
  assert.equal((await snapshot(key)).agent, 'implementer');
  await driver.stop(key);
  const log = new DatabaseSync(join(dataDir, sessionDirName(key), 'session.sqlite'), { readOnly: true });
  const entries = (log.prepare('SELECT record FROM entries').all() as Array<{ record: string }>).map((e) => e.record).join('\n');
  log.close();
  assert.match(entries, /echo: first/);
  assert.match(entries, /echo: second/);

  assert.equal((await invoke(key, { input: 'third', wait: true })).status, 200);
  assert.equal((await snapshot(key)).agent, 'implementer');
  assert.equal(await driver.agentOf(key), 'implementer');
});

test('a role switch on a session a page is watching stops its process and runs the new role', async () => {
  const { invoke, snapshot, base } = await start('echo-session.ts');
  const key = 'github:o/r#9';
  assert.equal((await invoke(key, { input: 'first', wait: true }, 'reviewer')).status, 200);
  const ws = new WebSocket(`${base.replace('http', 'ws')}/sessions/${encodeURIComponent(key)}/ws`);
  await new Promise((r) => ws.addEventListener('message', r, { once: true }));
  const closed = new Promise((r) => ws.addEventListener('close', r));
  assert.equal((await invoke(key, { input: 'second', wait: true }, 'implementer')).status, 200);
  assert.equal((await snapshot(key)).agent, 'implementer');
  await closed;
});

test('a submission naming another agent is refused while a run is open', async () => {
  const { invoke, snapshot, base } = await start('gate-session.ts');
  const key = 'github:o/r#8';
  await invoke(key, { input: 'review', requestId: 'r-1' }, 'reviewer');
  const get = (path: string) => fetch(`${base}/sessions/${encodeURIComponent(key)}${path}`).then((r) => r.json()) as Promise<Record<string, any>>;
  for (let i = 0; i < 750 && !(await get('/ping')).waitingOnHuman; i++) await new Promise((r) => setTimeout(r, 20));
  assert.equal((await get('/ping')).waitingOnHuman, true);

  assert.equal((await invoke(key, { input: 'fix' }, 'implementer')).status, 409);
  assert.equal((await snapshot(key)).agent, 'reviewer');
});
