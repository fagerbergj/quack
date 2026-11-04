import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { join } from 'node:path';
import { mock, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { agentSpec, buildModels, declaredEnv, loadAgent, openSession, parseDefinition, parseDeployment, resolveDeployment, resolveExtensions, type Catalog } from '../src/index.ts';

const deployment = {
  defaultModel: 'local/qwen3.8-27b',
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
    () => parseDeployment({ defaultModel: 'local/m', providers: { local: { baseUrl: 'ftp://x', models: { m: { contextWindow: 0 } } } } }),
    (err: Error) => /baseUrl must be an http\(s\) URL/.test(err.message) && /models\.m\.contextWindow must be a positive integer/.test(err.message),
  );
  assert.throws(() => parseDeployment({}), (err: Error) => /providers must be an object/.test(err.message) && /defaultModel must be/.test(err.message));
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

test('a pinned snapshot that selects a removed extension loads without it, with one warning', () => {
  const warn = mock.method(console, 'error', () => {});
  try {
    const def = parseDefinition({ name: 'reviewer', version: 6, status: 'promoted', model: 'local/qwen3.8-27b', prompt: 'prompt.md', extensions: { coding: {}, lsp: { servers: [] } } }, parseDeployment(deployment), catalog);
    assert.deepEqual(Object.keys(def.extensions), ['coding']);
    assert.equal(warn.mock.callCount(), 1);
    assert.match(String(warn.mock.calls[0].arguments[0]), /extension lsp was removed/);
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
  assert.throws(() => parseDeployment({ ...deployment, timeZone: 'Mars/Olympus' }), /timeZone must be an IANA zone/);
});

test('collects the problems each selected extension reports for its own config', () => {
  const deployment = parseDeployment({ defaultModel: 'local/omni', providers: { local: { baseUrl: 'http://127.0.0.1:1/v1', models: { omni: { contextWindow: 8192, maxTokens: 512 } } } } });
  const entry = Object.assign(() => [], { validate: (config: unknown, d: unknown) => [`bad ${JSON.stringify(config)} ${Object.keys((d as typeof deployment).providers)}`] });
  const agent = (extensions: object) => ({ name: 'v', version: 1, model: 'local/omni', extensions });
  assert.throws(() => parseDefinition(agent({ picky: { a: 1 } }), deployment, { picky: entry }), /bad {"a":1} local/);
  assert.doesNotThrow(() => parseDefinition(agent({}), deployment, { picky: entry }));
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
    const models = buildModels(parseDeployment({ defaultModel: 'local/omni', providers: { local: { baseUrl, apiKeyEnv: 'QUACK_VISION_TEST_KEY', models: { omni: { contextWindow: 8192, maxTokens: 512, images } } } } }));
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

test('retention must be an idleDays count', () => {
  const d = parseDeployment(deployment);
  const base = { name: 'r', version: 1, model: 'local/qwen3.8-27b', extensions: {} };
  assert.deepEqual(parseDefinition({ ...base, retention: { idleDays: 14 } }, d, catalog).retention, { idleDays: 14 });
  for (const retention of [{ idleDays: 0 }, { idleDays: '14' }, { days: 14 }, 14]) assert.throws(() => parseDefinition({ ...base, retention }, d, catalog), /retention must be/);
});

test('model prices default to free, reach the built model, and must be non-negative numbers', () => {
  const models = (price?: unknown) => parseDeployment({ defaultModel: 'local/m', providers: { local: { baseUrl: 'http://127.0.0.1:1/v1', models: { m: { contextWindow: 8192, maxTokens: 512, ...(price !== undefined && { price }) } } } } });
  assert.deepEqual(buildModels(models()).getModel('local', 'm')?.cost, { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 });
  assert.deepEqual(buildModels(models({ input: 0.5, output: 2 })).getModel('local', 'm')?.cost, { input: 0.5, output: 2, cacheRead: 0, cacheWrite: 0 });
  for (const bad of [{ input: -1 }, { input: '1' }, { tokens: 1 }, 3]) assert.throws(() => models(bad), /price must map/);
});

test('selecting an extension adds what it requires, transitively, and loads dependencies first', async () => {
  const ext = (name: string, extra: object = {}) => Object.assign((config: unknown) => [{ name: `${name}:${JSON.stringify(config)}` } as never], extra);
  const deps: Catalog = {
    workflows: ext('workflows', { optional: ['judge'] }),
    app: ext('app', { requires: ['mid'], optional: ['judge', 'other'] }),
    mid: ext('mid', { requires: ['workflows'] }),
    other: ext('other', { requires: ['workflows'] }),
    judge: ext('judge'),
  };
  assert.deepEqual(resolveExtensions(['app'], deps), { order: ['workflows', 'mid', 'app'], problems: [] });
  // Optional ones are ordered before it only when the agent enables them; they are never added.
  assert.deepEqual(resolveExtensions(['app', 'judge'], deps).order, ['judge', 'workflows', 'mid', 'app']);
  const def = parseDefinition({ name: 'a', version: 1, model: 'local/qwen3.8-27b', extensions: { app: { x: 1 }, workflows: { workflow: 'w' } } }, parseDeployment(deployment), deps);
  const spec = await agentSpec(def, deps);
  assert.deepEqual(spec.extensions!.map((e) => e.name), ['workflows:{"workflow":"w"}', 'mid:undefined', 'app:{"x":1}']);
});

test('a missing requirement or a cycle fails the definition with a clear message', () => {
  const deps: Catalog = { a: Object.assign(() => [], { requires: ['ghost'] }), b: Object.assign(() => [], { requires: ['c'] }), c: Object.assign(() => [], { requires: ['b'] }) };
  const d = parseDeployment(deployment);
  const base = { name: 'a', version: 1, model: 'local/qwen3.8-27b' };
  assert.throws(() => parseDefinition({ ...base, extensions: { a: {} } }, d, deps), /extension a requires ghost, which is not available/);
  assert.throws(() => parseDefinition({ ...base, extensions: { b: {} } }, d, deps), /extension dependency cycle: b -> c -> b/);
});

test('the root is closed: a removed or misspelled top-level key is rejected and named', () => {
  for (const key of ['imageModel', 'defualtModel']) assert.throws(() => parseDeployment({ ...deployment, [key]: 'local/omni' }), new RegExp(`unknown top-level key ${key}`));
  assert.throws(() => parseDeployment({ ...deployment, defaultModel: undefined }), /defaultModel must be "provider\/model"/);
  assert.throws(() => parseDeployment({ ...deployment, providers: {} }), /at least one provider/);
  assert.throws(() => parseDeployment({ ...deployment, providers: { local: { baseUrl: 'http://x/v1', slots: 0 } } }), /slots must be a positive integer/);
  assert.throws(() => parseDeployment({ ...deployment, providers: { local: { baseUrl: 'http://x/v1', models: { m: { images: null } } } } }), /images must be a boolean/);
});

test('stores and extensions are checked: kinds, postgres urlEnv, extension keys, and store references', () => {
  const ok = { stores: { main: { kind: 'postgres', urlEnv: 'QUACK_DATABASE_URL' }, local: { kind: 'sqlite' } }, extensions: { sleeper: { store: 'main', settings: { league: '1' } }, bare: {} } };
  assert.deepEqual(parseDeployment({ ...deployment, ...ok }).extensions?.bare, {});
  const bad = (extra: object) => () => parseDeployment({ ...deployment, ...extra });
  assert.throws(bad({ stores: { x: { kind: 'mysql' } } }), /stores\.x\.kind must be sqlite or postgres/);
  assert.throws(bad({ stores: { x: { kind: 'postgres' } } }), /stores\.x\.urlEnv must name/);
  assert.throws(bad({ stores: { x: { kind: 'sqlite', urlEnv: 'U' } } }), /stores\.x\.urlEnv is not a sqlite store key/);
  assert.throws(bad({ extensions: { s: { url: 'http://s', tokenEnv: 'T' } } }), (err: Error) => /extensions\.s\.url is not an extension key/.test(err.message) && /extensions\.s\.tokenEnv is not an extension key/.test(err.message));
  assert.throws(bad({ extensions: { s: { store: 'typo' } }, stores: ok.stores }), /extensions\.s\.store typo is not in stores/);
  assert.throws(bad({ extensions: { s: { settings: null } } }), /settings must be an object/);
});

test('an old file with a services section is rejected, naming the key and its replacement', () => {
  assert.throws(() => parseDeployment({ ...deployment, services: { sleeper: { url: 'http://s:1', tokenEnv: 'T' } } }), /unknown top-level key services: it was renamed extensions/);
});

const listing = (data: unknown[], ok = true) => (async () => ({ ok, json: async () => ({ data }) })) as unknown as typeof fetch;
const thin = { defaultModel: 'local/a', providers: { local: { baseUrl: 'http://x/v1', models: { a: { contextWindow: 4096 } } } } };

test('models merge: discovered ids plus hand-written ones, a hand-written field wins, a model without contextWindow is listed but not selectable', async () => {
  const fetched = listing([{ id: 'a', meta: { n_ctx: 8192 }, architecture: { input_modalities: ['text', 'image'] } }, { id: 'ids-only' }]);
  const resolved = await resolveDeployment(parseDeployment({ ...thin, providers: { local: { baseUrl: 'http://x/v1', models: { a: { contextWindow: 4096 }, peer: { contextWindow: 1000, images: false } } } } }), fetched);
  assert.deepEqual(resolved.providers.local.models, { a: { contextWindow: 4096, images: true }, 'ids-only': {}, peer: { contextWindow: 1000, images: false } });
  const built = buildModels(resolved);
  assert.deepEqual([built.getModel('local', 'a')?.input, built.getModel('local', 'peer')?.input, built.getModel('local', 'ids-only')], [['text', 'image'], ['text'], undefined]);
  const d = { name: 'x', version: 1, extensions: {} };
  assert.throws(() => parseDefinition({ ...d, model: 'local/ids-only' }, resolved, {}), /ids-only has no contextWindow/);
  assert.equal(parseDefinition(d, resolved, {}).model, 'local/a');
});

test('an endpoint that is missing or down leaves the hand-written models as the whole list', async () => {
  const down = (async () => { throw new Error('refused'); }) as unknown as typeof fetch;
  const warn = mock.method(console, 'error', () => {});
  for (const fetched of [down, listing([], false)]) assert.deepEqual((await resolveDeployment(parseDeployment(thin), fetched)).providers.local.models, { a: { contextWindow: 4096 } });
  assert.equal(warn.mock.callCount(), 2, 'a refusal and a non-ok status both say the list fell back');
  warn.mock.restore();
});

test('a classifier provider is not asked for /models', async () => {
  let asked = 0;
  const counting = (async () => ((asked += 1), { ok: true, json: async () => ({ data: [] }) })) as unknown as typeof fetch;
  await resolveDeployment(parseDeployment({ ...thin, providers: { ...thin.providers, clef: { baseUrl: 'http://clef/v1', api: 'systemone', models: { clef: { contextWindow: 8192 } } } } }), counting);
  assert.equal(asked, 1, 'only the local provider');
});

test('defaultModel must resolve to a selectable model', async () => {
  const bad = (defaultModel: string) => resolveDeployment(parseDeployment({ ...thin, defaultModel }), listing([{ id: 'ids-only' }]));
  await assert.rejects(bad('local/ids-only'), /defaultModel: .*no contextWindow/);
  await assert.rejects(bad('local/nope'), /defaultModel: model local\/nope is not configured/);
  await assert.rejects(bad('nope/a'), /not configured/);
});
