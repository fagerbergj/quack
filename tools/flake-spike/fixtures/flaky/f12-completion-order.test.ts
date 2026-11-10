import assert from 'node:assert/strict';
import { pbkdf2 } from 'node:crypto';
import { test } from 'node:test';
import { promisify } from 'node:util';
import { settleAll } from '../subjects/pool.ts';

const hash = promisify(pbkdf2);

test('every task result comes back', async () => {
  const task = (i: number) => async () => { await hash('pw', 'salt', 20_000 + i * 2_000, 32, 'sha256'); return i; };
  assert.deepEqual(await settleAll([task(0), task(1), task(2), task(3)]), [0, 1, 2, 3]);
});
