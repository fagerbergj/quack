import assert from 'node:assert/strict';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { openSession } from '@quack/session';
import { startPublisher } from '../src/index.ts';

const cleanups: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

type Mode = 'down' | 'unauthorized' | 'hold' | 'ok';

// A stand-in index: answers by mode, records the fact ids it accepted, and parks 'hold' requests until released.
async function stubIndex() {
  const state = { mode: 'down' as Mode, accepted: new Set<string>(), arrived: 0, release: () => {} };
  const held = new Promise<void>((r) => { state.release = r; });
  const server: Server = createServer(async (req, res) => {
    let body = '';
    for await (const c of req) body += c;
    if (state.mode === 'down') return void res.writeHead(503).end('{}');
    if (state.mode === 'unauthorized') return void res.writeHead(401).end('{}');
    if (state.mode === 'hold') { state.arrived++; await held; }
    for (const f of JSON.parse(body).facts) state.accepted.add(f.id);
    res.writeHead(200).end('{}');
  });
  await new Promise<void>((r) => server.listen(0, r));
  cleanups.push(() => new Promise((r) => server.close(r)));
  return { state, url: `http://127.0.0.1:${(server.address() as AddressInfo).port}` };
}

async function openDemo() {
  const dir = await mkdtemp(join(tmpdir(), 'quack-pub-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage('Done.')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  return openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id } } });
}

async function until(probe: () => boolean, ms = 15_000) {
  const deadline = Date.now() + ms;
  while (!probe()) {
    if (Date.now() > deadline) throw new Error('condition not reached');
    await new Promise((r) => setTimeout(r, 10));
  }
}

const meta = { key: 'chat:1', agent: 'a', version: 1 };

test('only the retry timer delivers facts once the index is back', async () => {
  const index = await stubIndex();
  const session = await openDemo();
  const publisher = await startPublisher(session, { url: index.url, token: 't', meta, retryMs: 20 });
  assert.equal(index.state.accepted.size, 0);
  index.state.mode = 'ok';
  await until(() => index.state.accepted.has('session'));
  await publisher.stop();
  await session.close();
});

test('shutdown during a retry-timer send waits for it, with no unhandled rejection', async () => {
  const index = await stubIndex();
  const session = await openDemo();
  const rejections: unknown[] = [];
  const onRejection = (e: unknown) => rejections.push(e);
  process.on('unhandledRejection', onRejection);
  try {
    const publisher = await startPublisher(session, { url: index.url, token: 't', meta, retryMs: 20 });
    index.state.mode = 'hold';
    await until(() => index.state.arrived > 0);
    // The index answers after shutdown has begun: a stop that does not wait lets session.close() win and the ack commit rejects.
    const stopping = publisher.stop();
    setTimeout(index.state.release, 100);
    await stopping;
    await session.close();
    await new Promise((r) => setImmediate(r));
    assert.deepEqual(rejections, []);
    assert.ok(index.state.accepted.has('session'), 'the in-flight send finished and was acknowledged');
  } finally {
    process.off('unhandledRejection', onRejection);
  }
});

test('a 4xx answer is logged as a permanent rejection', async (t) => {
  const index = await stubIndex();
  index.state.mode = 'unauthorized';
  const log = t.mock.method(console, 'error', () => {});
  const session = await openDemo();
  const publisher = await startPublisher(session, { url: index.url, token: 'wrong', meta, retryMs: 20 });
  await until(() => log.mock.callCount() > 0);
  assert.match(String(log.mock.calls[0].arguments[0]), /rejected the facts permanently/);
  await publisher.stop();
  await session.close();
});
