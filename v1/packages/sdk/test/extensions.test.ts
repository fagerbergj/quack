import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { checkedSettings, extensionDbPath, extensionEntry, readDeploymentFile, storeFor } from '../src/index.ts';

const file = { stores: { main: { kind: 'postgres' as const, urlEnv: 'PG_URL' }, local: { kind: 'sqlite' as const } }, extensions: { a: { store: 'local' }, b: { store: 'main' } } };

test('an extension reads its entry from the deployment file QUACK_CONFIG names', () => {
  const dir = mkdtempSync(join(tmpdir(), 'quack-sdk-'));
  writeFileSync(join(dir, 'd.json'), JSON.stringify(file));
  const read = readDeploymentFile({ QUACK_CONFIG: join(dir, 'd.json') });
  assert.equal(extensionEntry(read, 'a').store, 'local');
  assert.throws(() => extensionEntry(read, 'c'), /extensions\.c is missing/);
  assert.throws(() => readDeploymentFile({}), /QUACK_CONFIG is required/);
});

test('a store name picks sqlite or postgres, and a sqlite extension keeps <name>.db in the data dir', () => {
  assert.deepEqual(storeFor(file, undefined), { kind: 'sqlite' });
  assert.deepEqual(storeFor(file, 'main', { PG_URL: 'postgres://x' }), { kind: 'postgres', url: 'postgres://x' });
  assert.throws(() => storeFor(file, 'main', {}), /stores\.main\.urlEnv PG_URL is not set/);
  assert.equal(extensionDbPath(file, 'a', { QUACK_DATA_DIR: '/data' }), '/data/a.db');
  assert.throws(() => extensionDbPath(file, 'b', { QUACK_DATA_DIR: '/data', PG_URL: 'x' }), /postgres; b keeps SQLite only/);
  assert.throws(() => extensionDbPath(file, 'a', {}), /QUACK_DATA_DIR is required/);
});

test('settings pass through when their service accepts them, and every problem is reported otherwise', () => {
  const check = (s: unknown) => (s && typeof s === 'object' && 'n' in s ? [] : ['n is required', 'second']);
  assert.deepEqual(checkedSettings('x', { n: 1 }, check), { n: 1 });
  assert.throws(() => checkedSettings('x', undefined, check), /invalid extensions\.x\.settings:\n- n is required\n- second/);
});
