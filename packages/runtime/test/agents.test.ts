import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { loadAgent, parseDeployment } from '@quack/session';
import { catalog } from '../src/catalog.ts';
import { slotsFromEnv } from '@quack/slots';
import { sessionEnv } from '../src/session-env.ts';

const deployment = parseDeployment({
  defaultModel: 'local/qwen3.8-27b',
  providers: {
    local: { baseUrl: 'http://127.0.0.1:11436/v1', models: { 'qwen3.8-27b': { contextWindow: 262144, maxTokens: 16384 } } },
    clef: { baseUrl: 'http://127.0.0.1:11436/upstream/clef/v1', api: 'systemone', models: { clef: { contextWindow: 8192 } } },
  },
});

test('the shipped reviewer definition loads with its prompt, and the GitHub tools', async () => {
  process.env.QUACK_INDEX_URL = 'http://127.0.0.1:8080';
  process.env.QUACK_INDEX_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/reviewer/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You review one pull request/);
  const tools = agent.extensions!.flatMap((e) => (e.tools ?? []).map((t) => t.name));
  for (const name of ['checkout_pr', 'submit_review', 'bash', 'write']) assert.ok(tools.includes(name), `${name} in ${tools.join(', ')}`);
});

test('the shipped researcher definition loads with the research tools and the report judge', async () => {
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/researcher/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You answer one question with a sourced report/);
  const tools = agent.extensions!.flatMap((e) => (e.tools ?? []).map((t) => t.name));
  for (const name of ['web_search', 'web_fetch', 'submit_report', 'write']) assert.ok(tools.includes(name), `${name} in ${tools.join(', ')}`);
});

test('the shipped sleeper-lineup definition loads its prompt and binds the lineup workflow', async () => {
  process.env.QUACK_INDEX_URL = 'http://127.0.0.1:8080';
  process.env.QUACK_INDEX_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-lineup/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the lineup analyst/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

for (const [name, opening] of [['sleeper-trade', /^You are the trade analyst/], ['sleeper-trade-finder', /^You are the trade finder/]] as const) {
  test(`the shipped ${name} definition loads its prompt and binds its workflow`, async () => {
    process.env.QUACK_INDEX_URL = 'http://127.0.0.1:8080';
    process.env.QUACK_INDEX_TOKEN = 'x';
    const agent = await loadAgent(fileURLToPath(new URL(`../../../agents/${name}/agent.json`, import.meta.url)), deployment, catalog);
    assert.match(agent.instructions!, opening);
    assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
  });
}

test('the shipped sleeper-history definition loads its prompt and binds the history workflow', async () => {
  process.env.QUACK_INDEX_URL = 'http://127.0.0.1:8080';
  process.env.QUACK_INDEX_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-history/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the history analyst/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

test('the shipped sleeper-waivers definition loads its prompt and binds the waivers workflow', async () => {
  process.env.QUACK_INDEX_URL = 'http://127.0.0.1:8080';
  process.env.QUACK_INDEX_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-waivers/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the waiver scout/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

test('the shipped sleeper-digest definition loads its prompt and binds the digest workflow', async () => {
  process.env.QUACK_INDEX_URL = 'http://127.0.0.1:8080';
  process.env.QUACK_INDEX_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-digest/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the league reporter/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

test('the shipped sleeper-retro definition loads its prompt and binds the retro workflow', async () => {
  process.env.QUACK_INDEX_URL = 'http://127.0.0.1:8080';
  process.env.QUACK_INDEX_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-retro/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the league reporter/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

test('the shipped sleeper-trends definition loads its prompt and binds the trends workflow', async () => {
  process.env.QUACK_INDEX_URL = 'http://127.0.0.1:8080';
  process.env.QUACK_INDEX_TOKEN = 'x';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/sleeper-trends/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are the trend scout/);
  assert.ok(agent.extensions!.some((e) => e.name === 'quack.workflows' && (e as { dispatch?: unknown }).dispatch));
});

test('a session process inherits the deployment-level Langfuse variables whatever its agent selects', () => {
  const host = { PATH: '/bin', LANGFUSE_BASE_URL: 'http://lf', LANGFUSE_PUBLIC_KEY: 'pk', LANGFUSE_SECRET_KEY: 'sk' };
  const env = sessionEnv(fileURLToPath(new URL('../../../agents/reviewer/agent.json', import.meta.url)), host);
  assert.deepEqual(Object.keys(env).sort(), ['LANGFUSE_BASE_URL', 'LANGFUSE_PUBLIC_KEY', 'LANGFUSE_SECRET_KEY', 'PATH']);
});

const reviewer = fileURLToPath(new URL('../../../agents/reviewer/agent.json', import.meta.url));

test('a session process gets no per-service url or token: only what the host lists and the deployment file path', () => {
  const dir = mkdtempSync(join(tmpdir(), 'quack-env-'));
  writeFileSync(join(dir, 'deployment.json'), JSON.stringify(deployment));
  const host = { PATH: '/bin', HOME: '/h', QUACK_CONFIG: join(dir, 'deployment.json'), QUACK_EXA_API_KEY: 'exa', QUACK_ADMIN_TOKEN: 'admin', QUACK_OTHER_SECRET: 'l' };
  assert.deepEqual(Object.keys(sessionEnv(reviewer, host)).sort(), ['HOME', 'PATH', 'QUACK_CONFIG']);
});

test('the gate is on for a session that was handed the distributor and its launch credential, and off otherwise', () => {
  assert.equal(slotsFromEnv({ QUACK_INDEX_URL: 'http://d', QUACK_INDEX_TOKEN: 't' }).length, 1);
  assert.equal(slotsFromEnv(sessionEnv(undefined, { PATH: '/bin' })).length, 0);
});

test('the shipped quack definition loads with the home tools, offering every extension\'s actions', async () => {
  process.env.QUACK_INDEX_URL = 'http://127.0.0.1:8080';
  const agent = await loadAgent(fileURLToPath(new URL('../../../agents/quack/agent.json', import.meta.url)), deployment, catalog);
  assert.match(agent.instructions!, /^You are quack's front door/);
  const tools = agent.extensions!.flatMap((e) => (e.tools ?? []));
  assert.deepEqual(tools.map((t) => t.name).sort(), ['find_sessions', 'read_session', 'run_again', 'send_message', 'start', 'stop_session']);
  for (const name of ['review_pull_request', 'research_question', 'run_job']) assert.match(tools.find((t) => t.name === 'start')!.description, new RegExp(`- ${name}:`));
});
