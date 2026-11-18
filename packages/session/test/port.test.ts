import assert from 'node:assert/strict';
import { spawn, type ChildProcess } from 'node:child_process';
import { EventEmitter, once } from 'node:events';
import { test } from 'node:test';
import { childPort } from '../src/index.ts';

// The timer never fires unless a test fires it: no wall clock, so a slow runner cannot reach the timeout branch.
const never = () => () => {};
// A child that has not exited, reported nothing, and has a pid; the test drives its events.
const fake = (exit: { exitCode: number | null } = { exitCode: null }) => Object.assign(new EventEmitter(), { pid: 4242, signalCode: null, ...exit }) as unknown as ChildProcess;

test('a child that binds its own port reports the one it got', async () => {
  const c = spawn(process.execPath, ['-e', "require('node:http').createServer().listen(0, function () { process.send({ port: this.address().port }); })"], { stdio: ['ignore', 'ignore', 'ignore', 'ipc'] });
  const exited = once(c, 'exit');
  try {
    assert.ok((await childPort(c, 'child', 10_000, never)) > 0);
  } finally {
    c.kill('SIGKILL');
    await exited;
  }
});

test('a child that exits before reporting rejects with its exit code', async () => {
  const c = fake();
  const waiting = childPort(c, 'child', 10_000, never);
  Object.assign(c, { exitCode: 3 }).emit('exit', 3, null);
  await assert.rejects(waiting, /^Error: child exited with 3$/);
  await assert.rejects(childPort(fake({ exitCode: 3 }), 'child', 10_000, never), /^Error: child exited with 3$/, 'already gone when asked');
});

test('a child that never reports rejects at the timeout, naming its pid as still running', async () => {
  let fire = () => {};
  const waiting = childPort(fake(), 'child', 10_000, (fn) => ((fire = fn), () => {}));
  fire();
  await assert.rejects(waiting, { code: 'E_NO_PORT', message: 'child did not report a port within 10000 ms; pid 4242 is still running' });
});

test('a child that cannot be spawned rejects with the spawn error, not at the timeout', async () => {
  const c = spawn('/nonexistent-session', [], { stdio: ['ignore', 'ignore', 'ignore', 'ipc'] });
  await assert.rejects(childPort(c, 'child', 10_000, never), /^Error: child failed to start: spawn \/nonexistent-session ENOENT$/);
});
