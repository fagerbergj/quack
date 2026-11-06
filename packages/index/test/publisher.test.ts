import assert from 'node:assert/strict';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { observeGeneration } from '@quack/sdk';
import { openSession } from '@quack/session';
import { startPublisher } from '../src/index.ts';

const cleanups: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

type Mode = 'down' | 'unauthorized' | 'hold' | 'ok';

// A stand-in index: answers by mode, records the fact ids it accepted, and parks 'hold' requests until released.
async function stubIndex() {
  const state = { mode: 'down' as Mode, accepted: new Set<string>(), data: new Map<string, Record<string, unknown>>(), arrived: 0, release: () => {} };
  const held = new Promise<void>((r) => { state.release = r; });
  const server: Server = createServer(async (req, res) => {
    let body = '';
    for await (const c of req) body += c;
    if (state.mode === 'down') return void res.writeHead(503).end('{}');
    if (state.mode === 'unauthorized') return void res.writeHead(401).end('{}');
    if (state.mode === 'hold') { state.arrived++; await held; }
    for (const f of JSON.parse(body).facts) { state.accepted.add(f.id); state.data.set(f.id, f.data); }
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

// A workflow step's completion bypasses pi-durable's usage totals; the publisher adds it from the sdk hook.
const reply = (cost: number) => ({ role: 'assistant', content: [], usage: { input: 100, output: 20, cacheRead: 5, cacheWrite: 0, totalTokens: 125, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: cost } } }) as never;

for (const [name, cost] of [['a priced', 0.25], ['an unpriced', 0]] as const) {
  test(`${name} direct completion publishes its tokens and cost ${cost}`, async () => {
    const index = await stubIndex();
    index.state.mode = 'ok';
    const publisher = await startPublisher(await openDemo(), { url: index.url, token: 't', meta, pollMs: 60_000 });
    await observeGeneration({ messages: [] }, async () => reply(cost));
    await publisher.stop();
    assert.deepEqual(index.state.data.get('cost'), { total: cost, input: 100, output: 20, cacheRead: 5, cacheWrite: 0 });
  });
}

test('a direct-spend commit that fails once is retried, so the spend is still recorded', async () => {
  const index = await stubIndex();
  index.state.mode = 'ok';
  const session = await openDemo();
  let failures = 0;
  // Methods bind to the real conversation: its private fields do not survive a Proxy receiver.
  const root = new Proxy(session.root, { get: (t, k) => { const v = Reflect.get(t, k); return k === 'commit' ? (...a: unknown[]) => (failures-- > 0 ? Promise.reject(new Error('blip')) : (t.commit as Function)(...a)) : typeof v === 'function' ? v.bind(t) : v; } });
  const publisher = await startPublisher({ harness: session.harness, root }, { url: index.url, token: 't', meta, pollMs: 60_000 });
  failures = 1;
  await observeGeneration({ messages: [] }, async () => reply(0.25));
  await publisher.stop();
  assert.deepEqual(index.state.data.get('cost'), { total: 0.25, input: 100, output: 20, cacheRead: 5, cacheWrite: 0 });
});

test('loop and direct spend are summed into one cost fact', async () => {
  const index = await stubIndex();
  index.state.mode = 'ok';
  const session = await openDemo();
  const publisher = await startPublisher(session, { url: index.url, token: 't', meta, pollMs: 60_000 });
  await session.submit('Hi', 'c1', true);
  const usage = await session.harness.usage(BACKGROUND_CONTEXT);
  const loop = Object.values(usage.models).reduce((n, u) => n + u.input + u.output + u.cacheRead + u.cacheWrite, 0);
  assert.ok(loop > 0);
  await observeGeneration({ messages: [] }, async () => reply(0.25));
  await publisher.stop();
  const d = index.state.data.get('cost') as Record<string, number>;
  assert.equal(d.input + d.output + d.cacheRead + d.cacheWrite, loop + 125);
  assert.equal(d.total, 0.25);
});
