import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { PolicyExecutionEnv, shellEnv, type ExecPolicy } from '../src/index.ts';

const cwd = await mkdtemp(join(tmpdir(), 'quack-exec-'));
after(() => rm(cwd, { recursive: true, force: true }));

const source = { PATH: process.env.PATH, HOME: '/home/agent', LC_ALL: 'C', QUACK_GH_SERVICE_TOKEN: 'gh-secret', QUACK_EXA_API_KEY: 'exa-secret', QUACK_INDEX_TOKEN: 'index-secret', KEEP_ME: 'yes' };

async function run(command: string | string[], policy: ExecPolicy = {}, options: Parameters<PolicyExecutionEnv['exec']>[1] = {}) {
  let output = '';
  const env = new PolicyExecutionEnv({ cwd, policy, source });
  const result = await env.exec(command, { ...options, onOutput: (t) => { output += t; } }, ctx);
  return { result, output };
}

test('a shell sees the toolchain variables and none of the service secrets, even when the tool asks to inherit', async () => {
  const { output } = await run('env', {}, { inheritEnv: true, env: {} });
  assert.match(output, /^HOME=\/home\/agent$/m);
  assert.match(output, /^LC_ALL=C$/m);
  assert.doesNotMatch(output, /secret|QUACK_|KEEP_ME/);
});

test('the policy names extra variables, and a caller passes its own to one command', async () => {
  const { output } = await run('echo "$KEEP_ME $ASKED"', { env: ['KEEP_ME'] }, { env: { ASKED: 'also' } });
  assert.equal(output.trim(), 'yes also');
  assert.deepEqual(Object.keys(shellEnv({ A: '1', PATH: 'p', LC_X: 'x' })).sort(), ['LC_X', 'PATH']);
});

test('an argv command is confined the same way and keeps its arguments intact', async () => {
  const { result, output } = await run(['sh', '-c', 'echo "[$QUACK_EXA_API_KEY] $1"', 'sh', 'a b']);
  assert.ok(result.ok && result.value.exitCode === 0);
  assert.equal(output.trim(), '[] a b');
});

test('a command past the cap is killed with a note, and the model may only ask for less', async () => {
  const slow = await run('sleep 30', { timeoutSeconds: 1 }, { timeout: 9999 });
  assert.ok(!slow.result.ok && slow.result.error.code === 'timeout');
  assert.match(slow.output, /exceeded the 1 second timeout/);
  const shorter = await run('sleep 30', { timeoutSeconds: 60 }, { timeout: 1 });
  assert.match(shorter.output, /exceeded the 1 second timeout/);
});

test('an allocation past the address-space limit fails the command and the next one still runs', async () => {
  const grow = 's=aaaaaaaaaaaaaaaa; for i in $(seq 1 26); do s="$s$s"; done; echo grown';
  const limited = await run(grow, { limits: { addressSpaceMb: 256 } });
  assert.doesNotMatch(limited.output, /grown/);
  const next = await run('echo still-here', { limits: { addressSpaceMb: 256 } });
  assert.match(next.output, /still-here/);
});

test('a fork bomb hits the process limit: the command fails, the next one runs', { skip: process.getuid?.() === 0 && 'root ignores RLIMIT_NPROC' }, async () => {
  const bomb = await run('/bin/true; /bin/echo forked', { limits: { maxProcs: 1 } });
  assert.doesNotMatch(bomb.output, /^forked$/m);
  assert.match(bomb.output, /Resource temporarily unavailable/);
  assert.match((await run('echo still-here')).output, /still-here/);
});
