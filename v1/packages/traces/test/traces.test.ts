import assert from 'node:assert/strict';
import { createServer, type IncomingMessage } from 'node:http';
import type { AddressInfo } from 'node:net';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { num, obj, str } from '@earendil-works/pi-schema';
import { observeGeneration, onGeneration } from '@quack/sdk';
import { defineWorkflow, runWorkflow, workflowExtension } from '@quack/workflows';
import { agentSpec } from '@quack/session';
import { openSession } from '@quack/session';
import { tracesFromEnv, tracesSessionExtensions } from '../src/index.ts';
import { generationSpan, remember, Sender } from '../src/session.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-traces-'));
after(() => rm(dir, { recursive: true, force: true }));

type OtlpSpan = { traceId: string; startTimeUnixNano: string; endTimeUnixNano: string; status: { code: number; message?: string }; attributes: Array<{ key: string; value: { stringValue: string } }> };
type Otlp = { auth: string | undefined; version: string | undefined; spans: OtlpSpan[] };
const attrs = (span: OtlpSpan) => Object.fromEntries(span.attributes.map((a) => [a.key, a.value.stringValue]));

// A fake Langfuse: `first` resolves with the first OTLP request; `status` and `reply` are what it answers.
async function fakeLangfuse(status = 200, reply = '{}') {
  let resolve!: (r: Otlp) => void;
  const first = new Promise<Otlp>((r) => (resolve = r));
  const server = createServer(async (req: IncomingMessage, res) => {
    let text = '';
    for await (const chunk of req) text += chunk;
    assert.equal(req.url, '/api/public/otel/v1/traces');
    resolve({ auth: req.headers.authorization, version: req.headers['x-langfuse-ingestion-version'] as string, spans: JSON.parse(text).resourceSpans[0].scopeSpans[0].spans });
    res.writeHead(status).end(reply);
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

test('a generation sends one OTLP span with Langfuse generation attributes', async () => {
  const lf = await fakeLangfuse();
  const done = await generate(tracesSessionExtensions({ baseUrl: lf.url, publicKey: 'pk', secretKey: 'sk', sessionKey: 'sess-1', agentName: 'reviewer', agentVersion: '3' }), 'Hi there.');
  assert.equal(done.status === 'done' && done.answer, 'Hi there.');
  const { auth, version, spans } = await lf.first;
  assert.equal(auth, `Basic ${Buffer.from('pk:sk').toString('base64')}`);
  assert.equal(version, '4');
  assert.equal(spans.length, 1);
  const a = attrs(spans[0]);
  assert.equal(a['langfuse.observation.type'], 'generation');
  assert.equal(a['langfuse.session.id'], 'sess-1');
  assert.equal(a['langfuse.trace.name'], 'reviewer');
  assert.equal(a['langfuse.observation.model.name'], 'faux-1');
  assert.match(a['langfuse.observation.input'], /Say hi\./);
  assert.match(a['langfuse.observation.output'], /Hi there\./);
  const usage = JSON.parse(a['langfuse.observation.usage_details']);
  for (const k of ['input', 'output', 'cache_read', 'cache_write', 'total']) assert.equal(typeof usage[k], 'number', k);
  assert.equal(typeof JSON.parse(a['langfuse.observation.cost_details']).total, 'number');
  assert.equal(a['langfuse.observation.level'], 'DEFAULT');
  assert.deepEqual([a['langfuse.observation.metadata.agent'], a['langfuse.observation.metadata.version'], a['langfuse.observation.metadata.stopReason']], ['reviewer', '3', 'stop']);
  assert.match(spans[0].traceId, /^[0-9a-f]{32}$/);
  assert.ok(BigInt(spans[0].endTimeUnixNano) > BigInt(spans[0].startTimeUnixNano));
  assert.equal(spans[0].status.code, 1);
  await lf.close();
});

test('an error stop is level ERROR with the provider message', () => {
  const message = { ...fauxAssistantMessage(''), stopReason: 'error' as const, errorMessage: 'boom' };
  const span = generationSpan({ agentName: 'a' }, 'k', [], 1, message) as unknown as OtlpSpan;
  assert.equal(attrs(span)['langfuse.observation.level'], 'ERROR');
  assert.equal(attrs(span)['langfuse.observation.status_message'], 'boom');
  assert.deepEqual(span.status, { code: 2, message: 'boom' });
});

test('an outage or a rejection is reported once and never fails the generation', async () => {
  for (const [status, reply] of [[500, 'down'], [200, '{"partialSuccess":{"rejectedSpans":"2","errorMessage":"bad"}}']] as const) {
    const lf = await fakeLangfuse(status, reply);
    const reports: string[] = [];
    let reported!: () => void;
    const second = new Promise<void>((r) => (reported = r));
    const sender = new Sender(`${lf.url}/api/public/otel/v1/traces`, 'Basic x', (t) => (reports.push(t), reported()));
    sender.push([{ a: 1 }]);
    await lf.first;
    await second;
    await lf.close();
    sender.push([{ a: 2 }]); // connection refused now; still only the first report
    await new Promise((r) => setImmediate(r));
    assert.equal(reports.length, 1);
    assert.match(reports[0], /Langfuse did not accept spans/);
  }
  const lf = await fakeLangfuse(500);
  const done = await generate(tracesSessionExtensions({ baseUrl: lf.url, publicKey: 'pk', secretKey: 'sk' }), 'Still fine.');
  assert.equal(done.status === 'done' && done.answer, 'Still fine.');
  await lf.first;
  await lf.close();
});

test('with any of the three variables unset nothing is installed', async () => {
  const all = { LANGFUSE_BASE_URL: 'http://lf', LANGFUSE_PUBLIC_KEY: 'pk', LANGFUSE_SECRET_KEY: 'sk' };
  const names = async (env: Record<string, string>) => (await agentSpec({ name: 'plain', version: 1, model: 'swap/m', extensions: {} }, {}, [() => tracesFromEnv(env)])).extensions!.map((e) => e.name);
  assert.deepEqual(await names(all), ['traces']);
  assert.deepEqual(await names({}), []);
  assert.deepEqual(await names({ ...all, LANGFUSE_SECRET_KEY: '' }), []);
});

test('requests that never get a response are capped at 32, dropping the oldest', () => {
  const pending = new Map<number, number>();
  for (let i = 0; i < 40; i++) remember(pending, i, i);
  assert.equal(pending.size, 32);
  assert.deepEqual([pending.keys().next().value, [...pending.keys()].at(-1)], [8, 39]);
});

test('the queue keeps the newest 200 spans and sends them in batches of 20', async () => {
  const sizes: number[] = [];
  const ids: number[] = [];
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  let done!: () => void;
  const finished = new Promise<void>((r) => (done = r));
  const server = createServer(async (req, res) => {
    let text = '';
    for await (const chunk of req) text += chunk;
    const { spans } = JSON.parse(text).resourceSpans[0].scopeSpans[0];
    sizes.push(spans.length);
    ids.push(...spans.map((e: { id: number }) => e.id));
    if (sizes.length === 1) await gate;
    res.writeHead(200).end('{}');
    if (sizes.length === 11) done();
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const sender = new Sender(`http://127.0.0.1:${(server.address() as AddressInfo).port}/api/public/otel/v1/traces`, 'Basic x');
  sender.push([{ id: 0 }]); // in flight, held by the server
  sender.push(Array.from({ length: 300 }, (_, i) => ({ id: i + 1 })));
  release();
  await finished;
  assert.deepEqual(sizes, [1, ...Array(10).fill(20)]);
  assert.deepEqual([ids[1], ids.at(-1)], [101, 300]);
  await new Promise((r) => server.close(r));
});

// Collects every span a fake Langfuse receives; `until(n)` resolves once n have arrived.
async function collectingLangfuse() {
  const spans: OtlpSpan[] = [];
  const waiters: Array<[number, () => void]> = [];
  const server = createServer(async (req, res) => {
    let text = '';
    for await (const chunk of req) text += chunk;
    spans.push(...JSON.parse(text).resourceSpans[0].scopeSpans[0].spans);
    res.writeHead(200).end('{}');
    waiters.filter(([n]) => spans.length >= n).forEach(([, r]) => r());
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  return {
    spans,
    url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
    until: (n: number) => (spans.length >= n ? Promise.resolve() : new Promise<void>((r) => waiters.push([n, r]))),
    close: () => new Promise((r) => server.close(r)),
  };
}

const sayOk = { name: 'says-ok', version: 1, threshold: 1, criteria: [{ id: 'tone', checks: [{ id: 'says_ok', label: 'Does not say ok', weight: 'gate' as const, code: (a: string) => ({ met: a.includes('ok') ? 1 : 0, detail: 'the text must say ok' }) }] }] };
const note = { kind: 'test.note', path: 'note.md' };
const flow = defineWorkflow({
  name: 'traced',
  steps: [
    { name: 'decide', kind: 'model', schema: obj({ n: num() }, { open: true }), prompt: () => 'pick a number' },
    { name: 'card', kind: 'judged', schema: obj({ text: str() }, { open: true }), prompt: () => 'write the text', card: (_c, d) => d as object, judge: () => ({ options: { kind: note, rubric: sayOk, evidence: async () => ({}), maxRevisions: 1 } }) },
  ],
});

test('workflow model and judged steps each send their generations through the sdk hook', async () => {
  const lf = await collectingLangfuse();
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(['{"n": 1}', '{"text": "no"}', '{"text": "ok"}'].map((r) => fauxAssistantMessage(r)));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const extensions = [workflowExtension([flow]), ...tracesSessionExtensions({ baseUrl: lf.url, publicKey: 'pk', secretKey: 'sk', sessionKey: 'wf-1', agentName: 'sleeper' })];
  const s = await openSession({ dir: await mkdtemp(join(dir, 's-')), models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'be terse', extensions } });
  assert.equal((await runWorkflow(s.harness, s.root, flow, null)).status, 'completed');
  await s.close();
  await lf.until(3);
  const got = lf.spans.map(attrs);
  assert.equal(got.length, 3);
  assert.deepEqual(got.map((a) => a['langfuse.observation.type']), ['generation', 'generation', 'generation']);
  assert.match(got[0]['langfuse.observation.input'], /^\[\{"role":"system","content":"be terse"\}/);
  assert.match(got[0]['langfuse.observation.input'], /pick a number/);
  assert.match(got[2]['langfuse.observation.input'], /failed its checks/);
  assert.match(got[2]['langfuse.observation.output'], /ok/);
  assert.ok(got.every((a) => a['langfuse.session.id'] === 'wf-1' && a['langfuse.trace.name'] === 'sleeper'));
  await lf.close();
});

test('image data stays out of the span input', () => {
  const input = [{ role: 'user', content: [{ type: 'image', data: 'AAAA', mimeType: 'image/jpeg' }] }];
  const span = generationSpan({}, 'k', input, 1, fauxAssistantMessage('x')) as unknown as OtlpSpan;
  assert.doesNotMatch(attrs(span)['langfuse.observation.input'], /AAAA/);
});

test('observeGeneration without listeners returns the reply, and a throwing listener does not reach the caller', async () => {
  const reply = fauxAssistantMessage('x');
  assert.equal(await observeGeneration({ messages: [] }, async () => reply), reply);
  const off = onGeneration(() => { throw new Error('observer'); });
  assert.equal(await observeGeneration({ messages: [] }, async () => reply), reply);
  off();
});

test('a priced generation span carries the same cost parts the index sums', () => {
  const usage = { input: 10, output: 5, cacheRead: 100, cacheWrite: 0, totalTokens: 115, cost: { input: 0.1, output: 0.2, cacheRead: 0.01, cacheWrite: 0, total: 0.31 } };
  const span = generationSpan({ baseUrl: 'http://x', publicKey: 'p', secretKey: 's' } as never, 'k', [], 0, { role: 'assistant', content: [], model: 'm', provider: 'local', stopReason: 'stop', usage } as never);
  assert.deepEqual(JSON.parse(attrs(span as never)['langfuse.observation.cost_details']), { input: 0.1, output: 0.2, cache_read: 0.01, cache_write: 0, total: 0.31 });
});
