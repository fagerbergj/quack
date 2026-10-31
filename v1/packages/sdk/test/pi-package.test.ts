import assert from 'node:assert/strict';
import { test } from 'node:test';
import { piPackage, piShutdown } from '../src/index.ts';

test('a pi package loads with UI calls as no-ops, and its shutdown handlers run on piShutdown', async () => {
  const ext = await piPackage('./fixtures/pi-pkg', { from: import.meta.url });
  assert.deepEqual(ext.tools?.map((t) => t.name), ['echo_json']);
  assert.equal((globalThis as any).piPkgStopped, undefined);
  await piShutdown();
  assert.equal((globalThis as any).piPkgStopped, true);
});

test('a missing pi package names itself in the error', async () => {
  await assert.rejects(piPackage('no-such-pi-package', { from: import.meta.url }), /pi package no-such-pi-package:/);
});
