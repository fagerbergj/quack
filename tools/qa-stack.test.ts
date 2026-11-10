import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { test } from 'node:test';
import { freePort, startDistributor, startStack, teardown, type Parts } from './qa/stack.ts';

const listening = (url: string) => fetch(`${url}/version`).then(() => true, () => false);

test('the stack serves /version and its stub app, then leaves no listeners', async () => {
  const web = mkdtempSync(join(tmpdir(), 'qa-web-'));
  writeFileSync(join(web, 'index.html'), '<!doctype html><title>qa</title>');
  const stack = await startStack({ web, apps: ['schedules'] });
  assert.equal(await listening(stack.url), true);
  const res = await fetch(`${stack.url}/apps/schedules`);
  assert.equal(((await res.json()) as { schedules: unknown[] }).schedules.length, 2);
  await stack.stop();
  assert.equal(await listening(stack.url), false);
});

test('a distributor whose free port was taken before it listened is relaunched on another', async () => {
  // Another test taking the port between freePort and listen is what timed this file out in CI (run 42721150500).
  const squatter = createServer().listen(0, '127.0.0.1');
  await new Promise((r) => squatter.once('listening', r));
  const taken = (squatter.address() as { port: number }).port;
  const parts: Parts = { children: [], servers: [], tmp: mkdtempSync(join(tmpdir(), 'quack-qa-')) };
  const web = join(parts.tmp, 'web');
  mkdirSync(web);
  writeFileSync(join(web, 'index.html'), '<!doctype html><title>qa</title>');
  const config = join(parts.tmp, 'deployment.json');
  writeFileSync(config, JSON.stringify({ defaultModel: 'local/m', providers: { local: { baseUrl: 'http://127.0.0.1:9/v1', models: { m: { contextWindow: 8192, maxTokens: 1024 } } } } }));
  const ports = [taken];
  try {
    const env = { QUACK_DATA_DIR: parts.tmp, QUACK_AGENTS_DIR: resolve('agents'), QUACK_CONFIG: config, QUACK_WEB_DIR: web, QUACK_LOCAL_API_KEY: 'x' };
    const url = await startDistributor(parts, env, async () => ports.shift() ?? freePort());
    assert.notEqual(url, `http://127.0.0.1:${taken}`);
    assert.equal(await listening(url), true);
    // A later free port can be taken too under parallel test files; each loss is one more relaunch.
    assert.ok(parts.children.length >= 2, 'the first launch lost the port and a later one serves');
  } finally {
    await teardown(parts);
    squatter.closeAllConnections();
    await new Promise((r) => squatter.close(r));
  }
});
