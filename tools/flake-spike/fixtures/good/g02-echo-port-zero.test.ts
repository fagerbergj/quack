import assert from 'node:assert/strict';
import { test } from 'node:test';
import { startEcho } from '../subjects/server.ts';

test('the echo server answers with the request path on the port it bound', async () => {
  const { url, server } = await startEcho();
  try {
    assert.equal(await (await fetch(`${url}/a/b?c=1`)).text(), '/a/b?c=1');
  } finally {
    await new Promise((r) => server.close(r));
  }
});
