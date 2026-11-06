import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { validate } from '../src/validate.ts';

const config = {
  defaultModel: 'local/m',
  providers: {
    local: { baseUrl: 'http://127.0.0.1:1/v1', models: { m: { contextWindow: 8192, maxTokens: 1024 }, 'qwen3.8-27b': { images: true, contextWindow: 262144, maxTokens: 16384 } } },
    clef: { baseUrl: 'http://127.0.0.1:1/v1', api: 'systemone', models: { clef: { contextWindow: 8192, maxTokens: 1024 } } },
  },
};
// No provider lists models: the hand-written entries are the whole list.
const noList = (async () => ({ ok: false })) as unknown as typeof fetch;
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
  assert.deepEqual(await validate({ ...o, fetch: noList }), { loaded: 2, failures: [] });
});

test('a bundle parse error and a snapshot with an unknown extension fail with their paths', async () => {
  const o = layout({ 'deployment.json': config, 'agents/bad/agent.json': '{', 'data/s1/agent-def/agent.json': { ...agent, extensions: { bogus: {} } }, 'data/s2/agent-def/agent.json': agent });
  const r = await validate({ ...o, fetch: noList });
  assert.equal(r.loaded, 1);
  assert.equal(r.failures.length, 2);
  assert.match(r.failures[0], /agents\/bad\/agent\.json/);
  assert.match(r.failures[1], /data\/s1\/agent-def\/agent\.json.*unknown extension bogus/s);
});

test('the repo agents load without their service env set', async () => {
  const agentsDir = fileURLToPath(new URL('../../../agents', import.meta.url));
  const r = await validate({ ...layout({ 'deployment.json': config }), agentsDir, fetch: noList });
  assert.deepEqual(r.failures, []);
  assert.ok(r.loaded > 0);
});

test('deployment-level extension env gets a placeholder too, so the slots gate is built', async () => {
  delete process.env.QUACK_INDEX_URL;
  delete process.env.QUACK_INDEX_TOKEN;
  const r = await validate({ ...layout({ 'deployment.json': config }), fetch: noList });
  assert.deepEqual(r.failures, []);
  assert.equal(process.env.QUACK_INDEX_URL, 'placeholder');
  assert.equal(process.env.QUACK_INDEX_TOKEN, 'placeholder');
});

test('a defaultModel no provider can serve fails validation before any agent loads', async () => {
  const o = layout({ 'deployment.json': { ...config, defaultModel: 'local/ghost' } });
  await assert.rejects(validate({ ...o, fetch: noList }), /defaultModel: model local\/ghost is not configured/);
});

test('extension settings are checked by the settings check each extension publishes, and a name nothing mounts is refused', async () => {
  const withExtensions = (extensions: object) => layout({ 'deployment.json': { ...config, extensions } });
  const ok = { sleeper: { settings: { league: '1', owner: 'me' } }, schedules: { settings: { retentionSweep: true } }, slots: {} };
  assert.deepEqual((await validate({ ...withExtensions(ok), fetch: noList })).failures, []);
  await assert.rejects(validate({ ...withExtensions({ sleeper: {} }), fetch: noList }), /extensions\.sleeper\.settings: league must be a non-empty string/);
  await assert.rejects(validate({ ...withExtensions({ schedules: { settings: { retentionSwep: true } } }), fetch: noList }), /extensions\.schedules\.settings: retentionSwep is not a schedules setting/);
  await assert.rejects(validate({ ...withExtensions({ slots: { settings: { x: 1 } } }), fetch: noList }), /extensions\.slots\.settings: slots takes no settings/);
  await assert.rejects(validate({ ...withExtensions({ ghost: {} }), fetch: noList }), /extensions\.ghost: no extension named ghost mounts an app/);
});

test('a deployment file that still has a services section is rejected, naming the key', async () => {
  await assert.rejects(validate({ ...layout({ 'deployment.json': { ...config, services: { sleeper: { url: 'http://x' } } } }), fetch: noList }), /unknown top-level key services: it was renamed extensions/);
});
