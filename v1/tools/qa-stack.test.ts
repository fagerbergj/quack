import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { startStack } from './qa/stack.ts';

const listening = (url: string) => fetch(`${url}/version`).then(() => true, () => false);

test('the stack serves /version and its stub app, then leaves no listeners', async () => {
  const web = mkdtempSync(join(tmpdir(), 'qa-web-'));
  writeFileSync(join(web, 'index.html'), '<!doctype html><title>qa</title>');
  const stack = await startStack({ web, apps: { schedules: 'stub' } });
  assert.equal(await listening(stack.url), true);
  const res = await fetch(`${stack.url}/apps/schedules`);
  assert.equal(((await res.json()) as { schedules: unknown[] }).schedules.length, 2);
  await stack.stop();
  assert.equal(await listening(stack.url), false);
});
