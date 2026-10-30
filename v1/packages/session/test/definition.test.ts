import assert from 'node:assert/strict';
import { test } from 'node:test';
import { agentSpec, buildModels, parseDefinition, parseDeployment, type Catalog } from '../src/index.ts';

const deployment = {
  providers: { local: { baseUrl: 'http://127.0.0.1:11436/v1', apiKeyEnv: 'QUACK_LOCAL_KEY', models: { 'qwen3.8-27b': { contextWindow: 262144, maxTokens: 16384, reasoning: true } } } },
};
const catalog: Catalog = { coding: () => [], github: (config) => [{ name: `github:${JSON.stringify(config)}` } as never] };

test('loads a valid deployment and definition into models and an agent spec', () => {
  const d = parseDeployment(deployment);
  const def = parseDefinition({ name: 'reviewer', version: 3, model: 'local/qwen3.8-27b', maxTurns: 60, extensions: { coding: {}, github: { label: 'quack:review' } } }, d, catalog);
  const spec = agentSpec(def, catalog);
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
