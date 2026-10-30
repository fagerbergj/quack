import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createDistributor, processDriver } from '../src/index.ts';

const dataDir = await mkdtemp(join(tmpdir(), 'quack-dist-'));
const driver = processDriver({ command: [process.execPath, fileURLToPath(new URL('./fixtures/echo-session.ts', import.meta.url))], dataDir });
const server = createDistributor(driver, { idleMs: 300 }).listen(0);
await new Promise((r) => server.once('listening', r));
const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;

after(async () => {
  await Promise.all(driver.running().map((k) => driver.stop(k)));
  await new Promise((r) => server.close(r));
  await rm(dataDir, { recursive: true, force: true });
});

const invoke = (key: string, body: unknown) =>
  fetch(`${base}/sessions/${encodeURIComponent(key)}/invocations`, { method: 'POST', body: JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json() }));

async function until(probe: () => boolean, ms = 5000) {
  const deadline = Date.now() + ms;
  while (!probe()) {
    if (Date.now() > deadline) throw new Error('timed out');
    await new Promise((r) => setTimeout(r, 20));
  }
}

test('routes each session key to its own process', async () => {
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
  const key = 'github:fagerbergj/quack#1826';
  const before = driver.pid(key)!;
  process.kill(before, 'SIGKILL');
  await until(() => driver.pid(key) !== undefined && driver.pid(key) !== before);
  const retried = await invoke(key, { input: 'review it', requestId: 'a', wait: true });
  assert.equal(retried.body.answer, 'echo: review it');
});

test('idle sessions release their process and wake on the next request', async () => {
  await until(() => driver.running().length === 0);
  const woke = await invoke('research:spec-decoding', { input: 'again', wait: true });
  assert.equal(woke.body.answer, 'echo: again');
});
