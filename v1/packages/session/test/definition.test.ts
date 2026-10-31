import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { agentSpec, buildModels, loadAgent, openSession, parseDefinition, parseDeployment, type Catalog } from '../src/index.ts';

const deployment = {
  providers: { local: { baseUrl: 'http://127.0.0.1:11436/v1', apiKeyEnv: 'QUACK_LOCAL_KEY', models: { 'qwen3.8-27b': { contextWindow: 262144, maxTokens: 16384, reasoning: true } } } },
};
const catalog: Catalog = { coding: () => [], github: (config) => [{ name: `github:${JSON.stringify(config)}` } as never] };

test('loads a valid deployment and definition into models and an agent spec', async () => {
  const d = parseDeployment(deployment);
  const def = parseDefinition({ name: 'reviewer', version: 3, model: 'local/qwen3.8-27b', maxTurns: 60, extensions: { coding: {}, github: { label: 'quack:review' } } }, d, catalog);
  const spec = await agentSpec(def, catalog);
  assert.deepEqual(spec.model, { provider: 'local', modelId: 'qwen3.8-27b' });
  assert.equal(spec.maxTurns, 60);
  assert.deepEqual(spec.extensions!.map((e) => e.name), ['github:{"label":"quack:review"}']);
  const model = buildModels(d).getModel('local', 'qwen3.8-27b');
  assert.equal(model?.baseUrl, 'http://127.0.0.1:11436/v1');
  assert.equal(model?.contextWindow, 262144);
});

test('reports every problem in a deployment at once', () => {
  assert.throws(
    () => parseDeployment({ providers: { local: { baseUrl: 'ftp://x', models: { m: { contextWindow: 0 } } } } }),
    (err: Error) => /baseUrl must be an http\(s\) URL/.test(err.message) && /models\.m needs positive integer/.test(err.message),
  );
  assert.throws(() => parseDeployment({}), /providers object/);
});

test('reports every problem in a definition at once, naming the available extensions', () => {
  assert.throws(
    () => parseDefinition({ name: '', version: 0, model: 'local/llama', maxTurns: -1, extensions: { coding: {}, sleeper: {} } }, parseDeployment(deployment), catalog),
    (err: Error) =>
      [/name must be/, /version must be/, /model local\/llama is not configured/, /maxTurns must be/, /unknown extension sleeper; available: coding, github/].every((r) => r.test(err.message)),
  );
});

test('a definition loads its prompt file, and a reopened session takes the new prompt and model', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'quack-def-'));
  try {
    const path = join(dir, 'agent.json');
    await writeFile(path, JSON.stringify({ name: 'reviewer', version: 1, model: 'local/qwen3.8-27b', prompt: 'prompt.md', extensions: {} }));
    await writeFile(join(dir, 'prompt.md'), 'Review v1.');
    assert.equal((await loadAgent(path, parseDeployment(deployment), catalog)).instructions, 'Review v1.');

    const faux = fauxProvider({ models: [{ id: 'a' }, { id: 'b' }] });
    let secondSaw = '';
    faux.setResponses([fauxAssistantMessage('ok'), (c) => { secondSaw = JSON.stringify(c.messages); return fauxAssistantMessage('again'); }]);
    const models = createModels();
    models.setProvider(faux.provider);
    const sessionDir = join(dir, 'session');
    const first = await openSession({ dir: sessionDir, models, agent: { model: { provider: faux.provider.id, modelId: 'a' }, instructions: 'Review v1.' } });
    await first.submit('first question', 'h-1', true);
    await first.close();
    const second = await openSession({ dir: sessionDir, models, agent: { model: { provider: faux.provider.id, modelId: 'b' }, instructions: 'Review v2.' } });
    const agent = await second.root.agent(BACKGROUND_CONTEXT);
    assert.equal(agent.instructions, 'Review v2.');
    assert.equal(agent.model?.modelId, 'b');
    await second.submit('second question', 'h-2', true);
    assert.match(secondSaw, /first question/, 'the reopened session keeps its history');
    await second.close();
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});
