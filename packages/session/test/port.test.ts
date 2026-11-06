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
