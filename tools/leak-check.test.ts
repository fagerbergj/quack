import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { fileOf, leakReport } from './leak-check.ts';

const preload = join(import.meta.dirname, 'leak-check.ts');

function runFile(body: string) {
  const file = join(mkdtempSync(join(tmpdir(), 'quack-leak-')), 'a.test.ts');
  writeFileSync(file, `import { test } from 'node:test';\n${body}\n`);
  const { NODE_TEST_CONTEXT: _, ...env } = process.env;
  return spawnSync(process.execPath, ['--test', '--import', preload, file], { encoding: 'utf8', env: { ...env, QUACK_LEAK_GRACE_MS: '200' }, timeout: 60_000 });
}

test('a test file that leaves a server listening fails and names the handle', () => {
  const r = runFile("import { createServer } from 'node:http';\ntest('leaks', () => { createServer().listen(0); });");
  assert.equal(r.status, 1, r.stdout + r.stderr);
  assert.match(r.stdout + r.stderr, /still alive 0\.2s after its tests, held by .*TCPServerWrap/);
});

test('a test file that cleans up exits on its own', () => {
  const r = runFile("import { createServer } from 'node:http';\ntest('closes', async () => { const s = createServer().listen(0); await new Promise((r) => s.close(r)); });");
  assert.equal(r.status, 0, r.stdout + r.stderr);
});

test('a known leak is excused by its repo-relative path, and only that file', () => {
  const known = 'packages/x/test/leaky.test.ts';
  const list = { [known]: 'a fixture leak' };
  assert.equal(fileOf(['node', join(import.meta.dirname, '..', known)]), known);
  assert.equal(leakReport(known, ['TCPServerWrap'], list).code, 0);
  assert.equal(leakReport('packages/x/test/y.test.ts', ['TCPServerWrap'], list).code, 1);
});
