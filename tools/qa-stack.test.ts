import assert from 'node:assert/strict';
import { spawn, type ChildProcess } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { test } from 'node:test';
import { probe, startDistributor, startStack, teardown, type Parts } from './qa/stack.ts';

test('the stack serves /version and its stub app', async () => {
  const web = mkdtempSync(join(tmpdir(), 'qa-web-'));
  writeFileSync(join(web, 'index.html'), '<!doctype html><title>qa</title>');
  const stack = await startStack({ web, apps: ['schedules'] });
  const res = await fetch(`${stack.url}/apps/schedules`);
  assert.equal(res.status, 200);
  assert.equal(((await res.json()) as { schedules: unknown[] }).schedules.length, 2);
  // The gateway refuses a credential-free write and admits the page routes.
  assert.equal((await fetch(`${stack.url}/apps/schedules/x`, { method: 'PUT', body: '{}' })).status, 401);
  assert.equal((await fetch(`${stack.url}/apps/schedules/nope`, { method: 'DELETE' })).status, 404);
  await stack.stop();
});

test('teardown ends every child process group', async () => {
  // Not a port probe: a concurrent stack can take the freed port, so the groups are checked directly.
  const parts: Parts = { children: [], servers: [], tmp: mkdtempSync(join(tmpdir(), 'qa-td-')) };
  for (let i = 0; i < 2; i++) parts.children.push(spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { detached: true, stdio: 'ignore' }));
  const groups = parts.children.map((c) => c.pid!);
  await teardown(parts);
  for (const pid of groups) assert.throws(() => process.kill(-pid, 0), { code: 'ESRCH' });
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
  assert.notEqual(launched[0].QUACK_COMMIT, launched[1].QUACK_COMMIT);
});

test('the readiness probe accepts only the distributor launched with its nonce', async () => {
  // Another stack's distributor on the same port answers /version too, with its own (or no) commit.
  const answer = (commit: string | null) => createServer((_req, res) => res.end(JSON.stringify({ commit }))).listen(0, '127.0.0.1');
  const [mine, other] = [answer('n1'), answer(null)];
  const at = (s: ReturnType<typeof answer>) => `http://127.0.0.1:${(s.address() as AddressInfo).port}`;
  await Promise.all([mine, other].map((s) => new Promise((ok) => s.once('listening', ok))));
  try {
    assert.equal(await probe(at(mine), 'n1'), true);
    assert.equal(await probe(at(other), 'n1'), false);
  } finally {
    mine.close();
    other.close();
  }
});

test('a distributor that keeps exiting fails after three launches', async () => {
  let n = 0;
  const start = () => (n++, { exitCode: 1, signalCode: null }) as ChildProcess;
  await assert.rejects(startDistributor({ children: [], servers: [], tmp: '' }, {}, { start, serving: async () => false }), /3 times/);
  assert.equal(n, 3);
});
