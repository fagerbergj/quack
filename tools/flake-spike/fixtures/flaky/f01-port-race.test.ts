import assert from 'node:assert/strict';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { freePort, startEcho } from '../subjects/server.ts';

test('the echo server answers on the port it was configured with', async () => {
  const port = await freePort();
  const dir = await mkdtemp(join(tmpdir(), 'echo-'));
  await writeFile(join(dir, 'echo.json'), JSON.stringify({ port }));
  const { url, server } = await startEcho(port);
  try {
    assert.equal(url, `http://127.0.0.1:${port}`);
    assert.equal(await (await fetch(`${url}/hi`)).text(), '/hi');
  } finally {
    server.close();
  }
});
