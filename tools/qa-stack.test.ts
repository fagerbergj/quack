import assert from 'node:assert/strict';
import type { ChildProcess } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { startDistributor, startStack, type Parts } from './qa/stack.ts';

const listening = (url: string) => fetch(`${url}/version`).then(() => true, () => false);

test('the stack serves /version and its stub app, then leaves no listeners', async () => {
  const web = mkdtempSync(join(tmpdir(), 'qa-web-'));
  writeFileSync(join(web, 'index.html'), '<!doctype html><title>qa</title>');
  const stack = await startStack({ web, apps: ['schedules'] });
  assert.equal(await listening(stack.url), true);
  const res = await fetch(`${stack.url}/apps/schedules`);
  assert.equal(((await res.json()) as { schedules: unknown[] }).schedules.length, 2);
  // The gateway refuses a credential-free write and admits the page routes.
  assert.equal((await fetch(`${stack.url}/apps/schedules/x`, { method: 'PUT', body: '{}' })).status, 401);
  assert.equal((await fetch(`${stack.url}/apps/schedules/nope`, { method: 'DELETE' })).status, 404);
  await stack.stop();
  assert.equal(await listening(stack.url), false);
});

test('a distributor that exits before serving is relaunched on new ports', async () => {
  // Fakes stand in for the port race and the process: a real squatter's effect depends on the kernel (gVisor lets a wildcard listen share a loopback port).
  const parts: Parts = { children: [], servers: [], tmp: '' };
  const ports = [1, 2, 3, 4];
  const launched: Record<string, string>[] = [];
  const start = (_parts: Parts, _args: string[], env: Record<string, string>) => {
    launched.push(env);
    return (launched.length === 1 ? { exitCode: 1, signalCode: null } : { exitCode: null, signalCode: null }) as ChildProcess;
  };
  const url = await startDistributor(parts, {}, { next: async () => ports.shift()!, start, serving: async (u) => u.endsWith(':3') });
  assert.equal(url, 'http://127.0.0.1:3');
  assert.deepEqual(launched.map((e) => [e.PORT, e.QUACK_SESSION_PORT]), [['1', '2'], ['3', '4']]);
});

test('a distributor that keeps exiting fails after three launches', async () => {
  let n = 0;
  const start = () => (n++, { exitCode: 1, signalCode: null }) as ChildProcess;
  await assert.rejects(startDistributor({ children: [], servers: [], tmp: '' }, {}, { start, serving: async () => false }), /3 times/);
  assert.equal(n, 3);
});
