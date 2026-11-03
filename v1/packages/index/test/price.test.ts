import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { buildModels, openSession, parseDeployment, priceOf } from '@quack/session';
import { collectFacts, createStore, rebuild, sqliteBackend } from '../src/index.ts';

const ctx = BACKGROUND_CONTEXT;
const dir = await mkdtemp(join(tmpdir(), 'quack-price-'));
process.env.QUACK_LOCAL_API_KEY = 'unused';

// A local OpenAI-compatible server that answers every request with 1M prompt and 500k completion tokens.
const server = createServer((_req, res) => {
  res.writeHead(200, { 'content-type': 'text/event-stream' });
  const chunk = (o: object) => res.write(`data: ${JSON.stringify({ id: 'x', object: 'chat.completion.chunk', created: 0, model: 'm', ...o })}\n\n`);
  chunk({ choices: [{ index: 0, delta: { role: 'assistant', content: 'Done.' }, finish_reason: null }] });
  chunk({ choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] });
  chunk({ choices: [], usage: { prompt_tokens: 1_000_000, completion_tokens: 500_000, total_tokens: 1_500_000 } });
  res.end('data: [DONE]\n\n');
});
await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
after(async () => {
  server.close();
  await rm(dir, { recursive: true, force: true });
});

const configFor = (price?: object) => {
  const { port } = server.address() as { port: number };
  return parseDeployment({ providers: { local: { baseUrl: `http://127.0.0.1:${port}/v1`, models: { m: { contextWindow: 8192, maxTokens: 512, ...(price && { price }) } } } } });
};
const agent = { model: { provider: 'local', modelId: 'm' }, extensions: [] };

// Runs one session on a deployment priced `then`, closes it, and returns its dir.
async function ran(then?: object, sdir?: string) {
  const d = sdir ?? (await mkdtemp(join(dir, 's-')));
  const session = await openSession({ dir: d, models: buildModels(configFor(then)), agent });
  assert.equal((await session.submit('Hi.', 'c1', true)).status, 'done');
  await session.close();
  return d;
}

// Reopens the session as the runtime does and collects its facts at `now`'s prices.
async function costFact(sdir: string, now?: object) {
  const session = await openSession({ dir: sdir, models: buildModels(configFor(now)), agent });
  const facts = await collectFacts(session.harness, session.root, { key: 'k', agent: 'a', version: 1 }, 0, ctx, priceOf(configFor(now)));
  await session.close();
  return facts.find((f) => f.id === 'cost');
}

test('a priced local model costs its tokens at the configured per-million rates in the index fact', async () => {
  const fact = await costFact(await ran({ input: 0.3, output: 1.2 }), { input: 0.3, output: 1.2 });
  assert.equal(fact?.data.total, 0.9);
  assert.equal(fact?.data.input, 1_000_000);
});

test('calls made before a price existed show their full cost at the current price', async () => {
  const fact = await costFact(await ran(), { input: 0.3, output: 1.2 });
  assert.equal(fact?.data.total, 0.9);
});

test('a price change reprices recorded calls and moves the revision', async () => {
  const sdir = await ran({ input: 0.3, output: 1.2 });
  const before = await costFact(sdir, { input: 0.3, output: 1.2 });
  const after = await costFact(sdir, { input: 0.6, output: 2.4 });
  assert.equal(after?.data.total, 1.8);
  assert.ok(after!.rev > before!.rev);
});

test('an unpriced local model reports its tokens and a zero cost', async () => {
  const fact = await costFact(await ran());
  assert.equal(fact?.data.total, 0);
  assert.equal(fact?.data.output, 500_000);
});

test('rebuild reprices sessions whose calls were made before the price', async () => {
  const dataDir = await mkdtemp(join(dir, 'data-'));
  const sdir = join(dataDir, 'abc');
  await mkdir(sdir);
  await ran(undefined, sdir);
  await writeFile(join(sdir, 'session-key'), 'chat:1');
  const store = createStore(sqliteBackend(join(dir, 'index.sqlite')));
  await store.ensure();
  assert.equal(await rebuild(store, dataDir, Date.now, priceOf(configFor({ input: 0.3, output: 1.2 }))), 1);
  assert.equal((await store.facts(['cost']))[0]?.data.total, 0.9);
});
