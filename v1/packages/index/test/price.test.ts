import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { buildModels, openSession, parseDeployment } from '@quack/session';
import { collectFacts } from '../src/index.ts';

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

async function costFact(price?: object) {
  const { port } = server.address() as { port: number };
  const deployment = parseDeployment({ providers: { local: { baseUrl: `http://127.0.0.1:${port}/v1`, models: { m: { contextWindow: 8192, maxTokens: 512, ...(price && { price }) } } } } });
  const session = await openSession({ dir: await mkdtemp(join(dir, 's-')), models: buildModels(deployment), agent: { model: { provider: 'local', modelId: 'm' }, extensions: [] } });
  assert.equal((await session.submit('Hi.', 'c1', true)).status, 'done');
  const facts = await collectFacts(session.harness, session.root, { key: 'k', agent: 'a', version: 1 }, 0, ctx);
  await session.close();
  return facts.find((f) => f.id === 'cost')?.data;
}

test('a priced local model costs its tokens at the configured per-million rates in the index fact', async () => {
  const data = await costFact({ input: 0.3, output: 1.2 });
  assert.equal(data?.total, 0.3 + 0.6);
  assert.equal(data?.input, 1_000_000);
});

test('an unpriced local model reports its tokens and a zero cost', async () => {
  const data = await costFact();
  assert.equal(data?.total, 0);
  assert.equal(data?.output, 500_000);
});
