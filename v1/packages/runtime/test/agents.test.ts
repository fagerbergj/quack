import assert from 'node:assert/strict';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { loadAgent, parseDeployment } from '@quack/session';
import { catalog } from '../src/catalog.ts';
import { slotsFromEnv } from '@quack/slots';
import { sessionEnv } from '../src/session-env.ts';

const deployment = parseDeployment({
  providers: {
    local: { baseUrl: 'http://127.0.0.1:11436/v1', models: { 'qwen3.8-27b': { contextWindow: 262144, maxTokens: 16384 } } },
    clef: { baseUrl: 'http://127.0.0.1:11436/upstream/clef/v1', api: 'systemone', models: { clef: { contextWindow: 8192 } } },
  },
});

test('the shipped reviewer definition loads with its prompt, the GitHub tools and the language servers', async () => {
  process.env.QUACK_GH_SERVICE_URL = 'http://127.0.0.1:8090';
  process.env.QUACK_GH_SERVICE_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/reviewer/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You review one pull request/);
  const tools = agent.extensions!.flatMap((e) => (e.tools ?? []).map((t) => t.name));
  for (const name of ['checkout_pr', 'post_review', 'submit_review', 'bash', 'write', 'lsp_references', 'lsp_symbols']) assert.ok(tools.includes(name), `${name} in ${tools.join(', ')}`);
});

test('the shipped researcher definition loads with the research tools and the report judge', async () => {
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/researcher/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You answer one question with a sourced report/);
  const tools = agent.extensions!.flatMap((e) => (e.tools ?? []).map((t) => t.name));
  for (const name of ['web_search', 'web_fetch', 'submit_report', 'write']) assert.ok(tools.includes(name), `${name} in ${tools.join(', ')}`);
});

test('the shipped sleeper-lineup definition loads its prompt and binds the lineup workflow', async () => {
  process.env.QUACK_SLEEPER_URL = 'http://127.0.0.1:8092';
  process.env.QUACK_SLEEPER_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-lineup/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the lineup analyst/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

for (const [name, opening] of [['sleeper-trade', /^You are the trade analyst/], ['sleeper-trade-finder', /^You are the trade finder/]] as const) {
  test(`the shipped ${name} definition loads its prompt and binds its workflow`, async () => {
    process.env.QUACK_SLEEPER_URL = 'http://127.0.0.1:8092';
    process.env.QUACK_SLEEPER_TOKEN = 'x';
    const agent = await loadAgent(fileURLToPath(new URL(`../../../agents/${name}/agent.json`, import.meta.url)), deployment, catalog);
    assert.match(agent.instructions!, opening);
    assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
  });
}

test('the shipped sleeper-history definition loads its prompt and binds the history workflow', async () => {
  process.env.QUACK_SLEEPER_URL = 'http://127.0.0.1:8092';
  process.env.QUACK_SLEEPER_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-history/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the history analyst/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

test('the shipped sleeper-waivers definition loads its prompt and binds the waivers workflow', async () => {
  process.env.QUACK_SLEEPER_URL = 'http://127.0.0.1:8092';
  process.env.QUACK_SLEEPER_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-waivers/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the waiver scout/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

test('the shipped sleeper-digest definition loads its prompt and binds the digest workflow', async () => {
  process.env.QUACK_SLEEPER_URL = 'http://127.0.0.1:8092';
  process.env.QUACK_SLEEPER_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-digest/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the league reporter/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

test('the shipped sleeper-retro definition loads its prompt and binds the retro workflow', async () => {
  process.env.QUACK_SLEEPER_URL = 'http://127.0.0.1:8092';
  process.env.QUACK_SLEEPER_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-retro/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the league reporter/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

test('the shipped sleeper-trends definition loads its prompt and binds the trends workflow', async () => {
  process.env.QUACK_SLEEPER_URL = 'http://127.0.0.1:8092';
  process.env.QUACK_SLEEPER_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-trends/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the trend scout/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

test('a reviewer session process inherits its own service token and nothing else secret', () => {
  const host = { PATH: '/bin', HOME: '/h', QUACK_GH_SERVICE_URL: 'u', QUACK_GH_SERVICE_TOKEN: 'gh', QUACK_EXA_API_KEY: 'exa', QUACK_INDEX_TOKEN: 'idx', QUACK_SLEEPER_TOKEN: 's', LANGFUSE_SECRET_KEY: 'l' };
  const env = sessionEnv(fileURLToPath(new URL('../../../agents/reviewer/agent.json', import.meta.url)), host);
  assert.deepEqual(Object.keys(env).sort(), ['HOME', 'PATH', 'QUACK_GH_SERVICE_TOKEN', 'QUACK_GH_SERVICE_URL']);
});

test('a session process inherits the deployment-level slots variables whatever its agent selects, and slotsFromEnv still sees them', () => {
  const host = { PATH: '/bin', QUACK_SLOTS_URL: 'http://slots', QUACK_SLOTS_TOKEN: 'sl', QUACK_INDEX_TOKEN: 'idx' };
  const env = sessionEnv(fileURLToPath(new URL('../../../agents/reviewer/agent.json', import.meta.url)), host);
  assert.deepEqual(Object.keys(env).sort(), ['PATH', 'QUACK_SLOTS_TOKEN', 'QUACK_SLOTS_URL']);
  assert.equal(slotsFromEnv(env).length, 1);
  assert.equal(slotsFromEnv(sessionEnv(undefined, { PATH: '/bin' })).length, 0);
});
