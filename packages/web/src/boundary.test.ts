import { readdirSync, readFileSync } from 'node:fs';
import { expect, test } from 'vitest';

// The shell reaches extensions only through their registry and the Extension interface, so an extension can be removed by deleting its entry.
test('no core module imports an extension directly', () => {
  const dir = `${import.meta.dirname}/`;
  const core = readdirSync(dir).filter((f) => /\.tsx?$/.test(f) && !/\.test\.tsx?$/.test(f) && f !== 'test-harness.ts');
  const reaching = core.flatMap((f) => [...readFileSync(`${dir}${f}`, 'utf8').matchAll(/from '\.\/extensions\/([^']+)'/g)].map((m) => `${f} -> ${m[1]}`));
  expect(reaching.filter((r) => !r.endsWith('-> index.ts'))).toEqual([]);
});
