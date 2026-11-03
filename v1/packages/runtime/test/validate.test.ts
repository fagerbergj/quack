import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { validate } from '../src/validate.ts';

const config = {
  providers: {
    local: { baseUrl: 'http://127.0.0.1:1/v1', models: { m: { contextWindow: 8192, maxTokens: 1024 }, 'qwen3.8-27b': { images: true, contextWindow: 262144, maxTokens: 16384 } } },
    clef: { baseUrl: 'http://127.0.0.1:1/v1', api: 'systemone', models: { clef: { contextWindow: 8192, maxTokens: 1024 } } },
  },
};
const agent = { name: 'a', version: 1, model: 'local/m', extensions: {} };

function layout(files: Record<string, unknown>) {
  const root = mkdtempSync(join(tmpdir(), 'quack-validate-'));
  for (const [rel, body] of Object.entries(files)) {
    mkdirSync(dirname(join(root, rel)), { recursive: true });
    writeFileSync(join(root, rel), typeof body === 'string' ? body : JSON.stringify(body));
  }
  return { config: join(root, 'deployment.json'), agentsDir: join(root, 'agents'), dataDir: join(root, 'data') };
}

test('a good deployment loads every bundle and pinned snapshot', async () => {
  const o = layout({ 'deployment.json': config, 'agents/a/agent.json': agent, 'data/s1/agent-def/agent.json': agent });
  assert.deepEqual(await validate(o), { loaded: 2, failures: [] });
});

test('a bundle parse error and a snapshot with an unknown extension fail with their paths', async () => {
  const o = layout({ 'deployment.json': config, 'agents/bad/agent.json': '{', 'data/s1/agent-def/agent.json': { ...agent, extensions: { bogus: {} } }, 'data/s2/agent-def/agent.json': agent });
  const r = await validate(o);
  assert.equal(r.loaded, 1);
  assert.equal(r.failures.length, 2);
  assert.match(r.failures[0], /agents\/bad\/agent\.json/);
  assert.match(r.failures[1], /data\/s1\/agent-def\/agent\.json.*unknown extension bogus/s);
});

test('the repo agents load without their service env set', async () => {
  const agentsDir = fileURLToPath(new URL('../../../agents', import.meta.url));
  const r = await validate({ ...layout({ 'deployment.json': config }), agentsDir });
  assert.deepEqual(r.failures, []);
  assert.ok(r.loaded > 0);
});

test('deployment-level extension env gets a placeholder too, so the slots gate is built', async () => {
  delete process.env.QUACK_SLOTS_URL;
  delete process.env.QUACK_SLOTS_TOKEN;
  const r = await validate(layout({ 'deployment.json': config }));
  assert.deepEqual(r.failures, []);
  assert.equal(process.env.QUACK_SLOTS_URL, 'placeholder');
  assert.equal(process.env.QUACK_SLOTS_TOKEN, 'placeholder');
});
