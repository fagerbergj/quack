import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { test } from 'node:test';
import { childPort } from '../src/index.ts';

const child = (code: string) => spawn(process.execPath, ['-e', code], { stdio: ['ignore', 'ignore', 'ignore', 'ipc'] });

test('a child that binds its own port reports the one it got', async () => {
  const c = child("require('node:http').createServer().listen(0, function () { process.send({ port: this.address().port }); })");
  const port = await childPort(c, 'child', 10_000);
  assert.ok(port > 0);
  c.kill();
});

test('a child that exits before reporting rejects with its exit code', async () => {
  await assert.rejects(childPort(child('process.exit(3)'), 'child', 10_000), /^Error: child exited with 3$/);
});

test('a child that never reports rejects at the timeout, naming its pid as still running', async () => {
  let fire = () => {};
  const c = child('setInterval(() => {}, 1000)');
  const waiting = childPort(c, 'child', 10_000, (fn) => ((fire = fn), () => {}));
  fire();
  await assert.rejects(waiting, { code: 'E_NO_PORT', message: `child did not report a port within 10000 ms; pid ${c.pid} is still running` });
  c.kill();
});

test('a child that cannot be spawned rejects with the spawn error, not at the timeout', async () => {
  const c = spawn('/nonexistent-session', [], { stdio: ['ignore', 'ignore', 'ignore', 'ipc'] });
  await assert.rejects(childPort(c, 'child', 10_000, () => () => {}), /^Error: child failed to start: spawn \/nonexistent-session ENOENT$/);
});
