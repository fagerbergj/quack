import assert from 'node:assert/strict';
import { mkdtemp, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { takeLaunchEndpoint } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-launch-'));
after(() => rm(dir, { recursive: true, force: true }));

test('the launch endpoint is the listener URL and the token file, which is removed once read', async () => {
  const file = join(dir, 'listener-token');
  await writeFile(file, 'tok\n', { mode: 0o600 });
  assert.deepEqual(await takeLaunchEndpoint({ QUACK_SESSION_LISTENER_URL: 'http://d', QUACK_LAUNCH_TOKEN_FILE: file }), { url: 'http://d', token: 'tok' });
  assert.deepEqual(await readdir(dir), []);
});

test('no pair is a session without a publisher; half a pair or an empty file fails the boot', async () => {
  assert.equal(await takeLaunchEndpoint({}), undefined);
  await assert.rejects(takeLaunchEndpoint({ QUACK_SESSION_LISTENER_URL: 'http://d' }), /set together or not at all/);
  await writeFile(join(dir, 'empty'), '');
  await assert.rejects(takeLaunchEndpoint({ QUACK_SESSION_LISTENER_URL: 'http://d', QUACK_LAUNCH_TOKEN_FILE: join(dir, 'empty') }), /holds no token/);
});

test('the pre-rename pair still reads, for the one release the env contract allows', async () => {
  assert.deepEqual(await takeLaunchEndpoint({ QUACK_INDEX_URL: 'http://d', QUACK_INDEX_TOKEN: 't' }), { url: 'http://d', token: 't' });
  assert.equal(await takeLaunchEndpoint({ QUACK_INDEX_URL: 'http://d' }), undefined);
});
