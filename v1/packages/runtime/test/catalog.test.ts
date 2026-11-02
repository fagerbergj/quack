import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { declaredEnv, deploymentEnv } from '@quack/session';
import { catalog, deploymentExtensions } from '../src/catalog.ts';

// The hard-coded catalog this one replaced: its entry names, per-extension env, and the workflow env table.
const SLEEPER = ['QUACK_SLEEPER_URL', 'QUACK_SLEEPER_TOKEN', 'QUACK_EXA_API_KEY'];
const WORKFLOW_ENV: Record<string, string[]> = {
  ...Object.fromEntries(['lineup', 'waivers', 'trade-finder', 'trade', 'trends', 'history', 'digest', 'retro'].map((w) => [`sleeper.${w}`, SLEEPER])),
  'remarkable.ingest': ['QUACK_REMARKABLE_URL', 'QUACK_REMARKABLE_TOKEN'],
};
const OLD_ENV: Record<string, string[] | ((config: unknown) => string[])> = {
  github: ['QUACK_GH_SERVICE_URL', 'QUACK_GH_SERVICE_TOKEN'],
  research: ['QUACK_EXA_API_KEY'],
  workflows: (config) => WORKFLOW_ENV[(config as { workflow?: string })?.workflow ?? ''] ?? [],
};

const agentsDir = new URL('../../../agents/', import.meta.url);

test('the manifest-built catalog has the old entry names and deployment env', () => {
  assert.deepEqual(Object.keys(catalog).sort(), ['coding', 'github', 'lsp', 'research', 'workflows']);
  assert.deepEqual(deploymentEnv(deploymentExtensions), ['QUACK_SLOTS_URL', 'QUACK_SLOTS_TOKEN', 'LANGFUSE_BASE_URL', 'LANGFUSE_PUBLIC_KEY', 'LANGFUSE_SECRET_KEY']);
});

for (const dir of readdirSync(agentsDir)) {
  test(`${dir}: the manifest-built catalog declares the env the old one did`, () => {
    const extensions = JSON.parse(readFileSync(fileURLToPath(new URL(`${dir}/agent.json`, agentsDir)), 'utf8')).extensions ?? {};
    const expected = Object.entries(extensions).flatMap(([name, config]) => {
      const env = OLD_ENV[name];
      return typeof env === 'function' ? env(config) : env ?? [];
    });
    for (const name of Object.keys(extensions)) assert.ok(name in catalog, `${name} in catalog`);
    assert.deepEqual(declaredEnv(extensions, catalog), expected);
  });
}
