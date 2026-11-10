import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { freePort } from '../subjects/server.ts';

const child = fileURLToPath(new URL('../subjects/echo-child.ts', import.meta.url));

async function waitUp(url: string, deadline: number) {
  while (Date.now() < deadline) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {
      // not listening yet
    }
    await new Promise((r) => setTimeout(r, 20));
  }
  throw new Error(`${url} never came up`);
}

test('the spawned service comes up and answers ok', async () => {
  const port = await freePort();
  const proc = spawn(process.execPath, [child], { env: { ...process.env, PORT: String(port) }, stdio: 'ignore' });
  try {
    await waitUp(`http://127.0.0.1:${port}/`, Date.now() + 5_000);
    assert.equal(await (await fetch(`http://127.0.0.1:${port}/`)).text(), 'ok');
  } finally {
    proc.kill();
  }
});
