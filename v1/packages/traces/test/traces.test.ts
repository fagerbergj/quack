import assert from 'node:assert/strict';
import { createServer, type IncomingMessage } from 'node:http';
import type { AddressInfo } from 'node:net';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { agentSpec } from '@quack/session';
import { openSession } from '@quack/session';
import { tracesFromEnv, tracesSessionExtensions } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-traces-'));
after(() => rm(dir, { recursive: true, force: true }));

type Ingested = { auth: string | undefined; batch: Array<{ type: string; body: Record<string, any> }> };

// A fake Langfuse: `first` resolves with the first ingestion request; `status` is what it answers.
async function fakeLangfuse(status = 207) {
  let resolve!: (r: Ingested) => void;
  const first = new Promise<Ingested>((r) => (resolve = r));
  const server = createServer(async (req: IncomingMessage, res) => {
    let text = '';
    for await (const chunk of req) text += chunk;
    assert.equal(req.url, '/api/public/ingestion');
    resolve({ auth: req.headers.authorization, ...JSON.parse(text) });
    res.writeHead(status).end('{}');
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  return { first, url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, close: () => new Promise((r) => server.close(r)) };
}

async function generate(extensions: ReturnType<typeof tracesSessionExtensions>, text: string) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([async () => (await new Promise((r) => setTimeout(r, 20)), fauxAssistantMessage(text))]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: await mkdtemp(join(dir, 's-')), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
  const done = await session.submit('Say hi.', 'r1', true);
  await session.close();
  return done;
}

test('a generation sends one Langfuse generation event with model, input, output, usage, cost, latency and metadata', async () => {
  const lf = await fakeLangfuse();
  const done = await generate(tracesSessionExtensions({ baseUrl: lf.url, publicKey: 'pk', secretKey: 'sk', sessionKey: 'sess-1', agentName: 'reviewer', agentVersion: '3' }), 'Hi there.');
  assert.equal(done.status === 'done' && done.answer, 'Hi there.');
  const { auth, batch } = await lf.first;
  assert.equal(auth, `Basic ${Buffer.from('pk:sk').toString('base64')}`);
  const gens = batch.filter((e) => e.type === 'generation-create');
  assert.equal(gens.length, 1);
  const g = gens[0].body;
  assert.equal(g.traceId, 'sess-1');
  assert.equal(g.model, 'faux-1');
  assert.match(JSON.stringify(g.input), /Say hi\./);
  assert.match(JSON.stringify(g.output), /Hi there\./);
  assert.equal(typeof g.usageDetails.total, 'number');
  assert.equal(typeof g.costDetails.total, 'number');
  assert.ok(Date.parse(g.endTime) > Date.parse(g.startTime));
  assert.equal(g.metadata.agent, 'reviewer');
  assert.equal(g.metadata.version, '3');
  assert.deepEqual(batch.find((e) => e.type === 'trace-create')!.body, { id: 'sess-1', sessionId: 'sess-1', name: 'reviewer' });
  await lf.close();
});

test('an ingestion outage does not fail the generation', async () => {
  const lf = await fakeLangfuse(500);
  const done = await generate(tracesSessionExtensions({ baseUrl: lf.url, publicKey: 'pk', secretKey: 'sk' }), 'Still fine.');
  assert.equal(done.status === 'done' && done.answer, 'Still fine.');
  await lf.first;
  await lf.close();
  const dead = await generate(tracesSessionExtensions({ baseUrl: lf.url, publicKey: 'pk', secretKey: 'sk' }), 'Unreachable is fine too.');
  assert.equal(dead.status === 'done' && dead.answer, 'Unreachable is fine too.');
});

test('with any of the three variables unset nothing is installed', async () => {
  const all = { LANGFUSE_BASE_URL: 'http://lf', LANGFUSE_PUBLIC_KEY: 'pk', LANGFUSE_SECRET_KEY: 'sk' };
  const names = async (env: Record<string, string>) => (await agentSpec({ name: 'plain', version: 1, model: 'swap/m', extensions: {} }, {}, [() => tracesFromEnv(env)])).extensions!.map((e) => e.name);
  assert.deepEqual(await names(all), ['traces']);
  assert.deepEqual(await names({}), []);
  assert.deepEqual(await names({ ...all, LANGFUSE_SECRET_KEY: '' }), []);
});
