import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { join } from 'node:path';
import { mock, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { agentSpec, buildModels, declaredEnv, loadAgent, openSession, parseDefinition, parseDeployment, type Catalog } from '../src/index.ts';

const deployment = {
  providers: { local: { baseUrl: 'http://127.0.0.1:11436/v1', apiKeyEnv: 'QUACK_LOCAL_KEY', models: { 'qwen3.8-27b': { contextWindow: 262144, maxTokens: 16384, reasoning: true } } } },
};
const catalog: Catalog = { coding: () => [], github: (config) => [{ name: `github:${JSON.stringify(config)}` } as never] };

test('loads a valid deployment and definition into models and an agent spec', async () => {
  const d = parseDeployment(deployment);
  const def = parseDefinition({ name: 'reviewer', version: 3, model: 'local/qwen3.8-27b', extensions: { coding: {}, github: { label: 'quack:review' } } }, d, catalog);
  const spec = await agentSpec(def, catalog);
  assert.deepEqual(spec.model, { provider: 'local', modelId: 'qwen3.8-27b' });
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
    () => parseDefinition({ name: '', version: 0, model: 'local/llama', extensions: { coding: {}, sleeper: {} } }, parseDeployment(deployment), catalog),
    (err: Error) =>
      [/name must be/, /version must be/, /model local\/llama is not configured/, /unknown extension sleeper; available: coding, github/].every((r) => r.test(err.message)),
  );
});

test('a pinned snapshot that still has maxTurns loads, with one warning', () => {
  const warn = mock.method(console, 'error', () => {});
  try {
    const def = parseDefinition({ name: 'reviewer', version: 1, status: 'promoted', model: 'local/qwen3.8-27b', maxTurns: 80, prompt: 'prompt.md', extensions: { coding: {} } }, parseDeployment(deployment), catalog);
    assert.equal(def.name, 'reviewer');
    assert.equal(warn.mock.callCount(), 1);
    assert.match(String(warn.mock.calls[0].arguments[0]), /maxTurns was removed/);
  } finally {
    warn.mock.restore();
  }
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

test('rejects an unknown deployment time zone', () => {
  assert.throws(() => parseDeployment({ providers: {}, timeZone: 'Mars/Olympus' }), /timeZone must be an IANA zone/);
});

const visionDeployment = (images?: boolean) =>
  parseDeployment({ providers: { local: { baseUrl: 'http://127.0.0.1:1/v1', models: { omni: { contextWindow: 8192, maxTokens: 512, ...(images && { images }) } } } } });
const visionAgent = (modelId: string) => ({ name: 'v', version: 1, model: 'local/omni', extensions: { workflows: { visionModel: { provider: 'local', modelId } } } });
const workflowsCatalog: Catalog = { workflows: () => [] };

test('refuses a vision model that is not declared with images: true', () => {
  assert.doesNotThrow(() => parseDefinition(visionAgent('omni'), visionDeployment(true), workflowsCatalog));
  assert.throws(() => parseDefinition(visionAgent('omni'), visionDeployment(), workflowsCatalog), /visionModel local\/omni must be configured .* images: true/);
  assert.throws(() => parseDefinition(visionAgent('missing'), visionDeployment(true), workflowsCatalog), /visionModel local\/missing/);
});

// Real pi-ai conversion: capture the chat-completions request body a model actually sends.
async function sentBody(images: boolean): Promise<string> {
  const bodies: string[] = [];
  const server = createServer((req, res) => {
    let body = '';
    req.on('data', (c) => (body += c));
    req.on('end', () => {
      bodies.push(body);
      res.writeHead(200, { 'content-type': 'text/event-stream' }).end('data: [DONE]\n\n');
    });
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  try {
    const baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1`;
    process.env.QUACK_VISION_TEST_KEY = 'k';
    const models = buildModels(parseDeployment({ providers: { local: { baseUrl, apiKeyEnv: 'QUACK_VISION_TEST_KEY', models: { omni: { contextWindow: 8192, maxTokens: 512, images } } } } }));
    const content = [{ type: 'text' as const, text: 'Page 1.' }, { type: 'image' as const, data: 'AAAA', mimeType: 'image/jpeg' }];
    await models.completeSimple(models.getModel('local', 'omni')!, { messages: [{ role: 'user', content, timestamp: 0 }] });
  } finally {
    server.close();
  }
  return bodies.join('');
}

test('an images: true model sends the image block; a text-only model sends a placeholder', async () => {
  assert.match(await sentBody(true), /data:image\/jpeg;base64,AAAA/);
  const textOnly = await sentBody(false);
  assert.doesNotMatch(textOnly, /base64,AAAA/);
  assert.match(textOnly, /image/i);
});

test('an agent receives only the variables its extensions declare, and exec settings are validated', async () => {
  const entry = Object.assign(() => [], { env: ['A_TOKEN'] });
  const withEnv: Catalog = { coding: () => [], github: entry, flow: Object.assign(() => [], { env: (c: unknown) => [`${(c as { w: string }).w}_KEY`] }) };
  assert.deepEqual(declaredEnv({ coding: {}, github: {}, flow: { w: 'X' } }, withEnv), ['A_TOKEN', 'X_KEY']);
  assert.deepEqual(declaredEnv({ coding: {} }, withEnv), []);
  assert.throws(() => parseDeployment({ ...deployment, exec: { timeoutSeconds: 0, limits: { maxProcs: -1 } } }), (err: Error) => /exec.timeoutSeconds/.test(err.message) && /exec.limits/.test(err.message));
  const d = parseDeployment({ ...deployment, exec: { timeoutSeconds: 120, env: ['GOPATH'] } });
  const def = parseDefinition({ name: 'r', version: 1, model: 'local/qwen3.8-27b', extensions: {}, exec: { timeoutSeconds: 30 } }, d, withEnv);
  assert.equal((await agentSpec(def, withEnv)).commandTimeoutSeconds, 30);
  assert.throws(() => parseDefinition({ name: 'r', version: 1, model: 'local/qwen3.8-27b', extensions: {}, exec: { timeoutSeconds: 'x' } }, d, withEnv), /exec.timeoutSeconds/);
  assert.throws(() => parseDefinition({ name: 'r', version: 1, model: 'local/qwen3.8-27b', extensions: {}, exec: { limits: {}, env: ['A'] } }, d, withEnv), (err: Error) => /exec.limits is set by the deployment/.test(err.message) && /exec.env is set by the deployment/.test(err.message));
  assert.throws(() => parseDeployment({ ...deployment, exec: { limits: { maxFileSize: 5 } } }), /exec.limits must map/);
});
