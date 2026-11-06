import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { test } from 'node:test';
import { bundleLocks } from './bundle-locks.ts';

test('every agent bundle matches its version.lock; changing content needs a version bump', async () => {
  for (const { dir, lock } of await bundleLocks()) {
    const committed = await readFile(join(dir, 'version.lock'), 'utf8').catch(() => '');
    assert.equal(committed, lock, `${dir} changed without a version bump: raise "version" in agent.json, then run node tools/bundle-locks.ts`);
  }
});
