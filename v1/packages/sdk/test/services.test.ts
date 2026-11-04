import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { checkedSettings, readDeploymentFile, serviceDbPath, serviceEntry, serviceToken, storeFor } from '../src/index.ts';

const file = { stores: { main: { kind: 'postgres' as const, urlEnv: 'PG_URL' }, local: { kind: 'sqlite' as const } }, services: { a: { url: 'http://a', tokenEnv: 'A_T', store: 'local' }, b: { url: 'http://b', store: 'main' } } };

test('a service reads its entry and token from the deployment file QUACK_CONFIG names', () => {
  const dir = mkdtempSync(join(tmpdir(), 'quack-sdk-'));
  writeFileSync(join(dir, 'd.json'), JSON.stringify(file));
  const env = { QUACK_CONFIG: join(dir, 'd.json'), A_T: 'tok' };
  const read = readDeploymentFile(env);
  assert.equal(serviceEntry(read, 'a').url, 'http://a');
  assert.equal(serviceToken(serviceEntry(read, 'a'), 'a', env), 'tok');
  assert.throws(() => serviceToken(serviceEntry(read, 'a'), 'a', {}), /services\.a\.tokenEnv A_T is not set/);
  assert.throws(() => serviceToken(serviceEntry(read, 'b'), 'b', env), /services\.b\.tokenEnv is required/);
  assert.throws(() => serviceEntry(read, 'c'), /services\.c is missing/);
  assert.throws(() => readDeploymentFile({}), /QUACK_CONFIG is required/);
});

test('a store name picks sqlite or postgres, and a sqlite service keeps <name>.db in the data dir', () => {
  assert.deepEqual(storeFor(file, undefined), { kind: 'sqlite' });
  assert.deepEqual(storeFor(file, 'main', { PG_URL: 'postgres://x' }), { kind: 'postgres', url: 'postgres://x' });
  assert.throws(() => storeFor(file, 'main', {}), /stores\.main\.urlEnv PG_URL is not set/);
  assert.equal(serviceDbPath(file, 'a', { QUACK_DATA_DIR: '/data' }), '/data/a.db');
  assert.throws(() => serviceDbPath(file, 'b', { QUACK_DATA_DIR: '/data', PG_URL: 'x' }), /postgres; b keeps SQLite only/);
  assert.throws(() => serviceDbPath(file, 'a', {}), /QUACK_DATA_DIR is required/);
});

test('settings pass through when their service accepts them, and every problem is reported otherwise', () => {
  const check = (s: unknown) => (s && typeof s === 'object' && 'n' in s ? [] : ['n is required', 'second']);
  assert.deepEqual(checkedSettings('x', { n: 1 }, check), { n: 1 });
  assert.throws(() => checkedSettings('x', undefined, check), /invalid services\.x\.settings:\n- n is required\n- second/);
});
