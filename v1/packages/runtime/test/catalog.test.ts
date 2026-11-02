import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { declaredEnv, deploymentEnv } from '@quack/session';
import { catalog, deploymentExtensions, loadManifests, buildCatalog } from '../src/catalog.ts';

// The hard-coded catalog this one replaced: its entry names, per-extension env, and the workflow env table.
const SLEEPER = ['QUACK_SLEEPER_URL', 'QUACK_SLEEPER_TOKEN', 'QUACK_EXA_API_KEY'];
const WORKFLOW_ENV: Record<string, string[]> = {
  ...Object.fromEntries(['lineup', 'waivers', 'trade-finder', 'trade', 'trends', 'history', 'digest', 'retro'].map((w) => [`sleeper.${w}`, SLEEPER])),
  'remarkable.ingest': ['QUACK_REMARKABLE_URL', 'QUACK_REMARKABLE_TOKEN'],
};
const OLD_ENV: Record<string, string[] | ((config: unknown) => string[])> = {
  github: ['QUACK_GH_SERVICE_URL', 'QUACK_GH_SERVICE_TOKEN'],
  research: ['QUACK_EXA_API_KEY'],
  home: ['QUACK_SLEEPER_URL', 'QUACK_SLEEPER_TOKEN', 'QUACK_INDEX_URL'],
  workflows: (config) => WORKFLOW_ENV[(config as { workflow?: string })?.workflow ?? ''] ?? [],
};

const agentsDir = new URL('../../../agents/', import.meta.url);

test('the manifest-built catalog has the old entry names and deployment env', () => {
  assert.deepEqual(Object.keys(catalog).sort(), ['coding', 'github', 'home', 'lsp', 'research', 'workflows']);
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

test('every loaded manifest\'s actions reach the home extension, and a name two extensions declare is refused', async () => {
  const manifests = await loadManifests();
  assert.deepEqual(manifests.flatMap((m) => m.actions ?? []).map((a) => a.name).sort(), ['research_question', 'review_pull_request', 'run_job']);
  const dup = { name: 'dup', actions: [{ name: 'run_job', summary: 's', params: {} as never, start: async () => ({ session: 'k', title: 't' }) }] };
  assert.throws(() => buildCatalog([...manifests, dup]), /action run_job is declared by two extensions/);
  const seen: string[][] = [];
  const probe = { name: 'probe', session: (_c: unknown, _w: unknown, actions: Array<{ name: string }>) => (seen.push(actions.map((a) => a.name)), []) };
  await buildCatalog([...manifests, probe]).catalog.probe({});
  assert.deepEqual(seen[0].sort(), ['research_question', 'review_pull_request', 'run_job']);
});

test('the workflows entry refuses a vision model not declared with images: true', () => {
  const deployment = (images?: boolean) => ({ providers: { local: { baseUrl: 'http://x/v1', models: { omni: { contextWindow: 8192, maxTokens: 512, ...(images && { images }) } } } } });
  const config = (modelId: string) => ({ visionModel: { provider: 'local', modelId } });
  const validate = catalog.workflows.validate!;
  assert.deepEqual(validate(config('omni'), deployment(true)), []);
  assert.match(validate(config('omni'), deployment())[0], /visionModel local\/omni must be configured .* images: true/);
  assert.match(validate(config('missing'), deployment(true))[0], /visionModel local\/missing/);
  assert.deepEqual(validate({ workflow: 'x' }, deployment()), []);
});
