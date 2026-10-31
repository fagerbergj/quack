import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineDoc, defineExtension, defineTool } from '@earendil-works/pi-durable';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { artifactsExtension, attachVerdict, seal, type ArtifactKind } from '@quack/sdk';
import { openSession } from '@quack/session';
import { itemFrom, langfuse, publish, readSession, replay, scoresOf, type ReplayResult } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

const note: ArtifactKind = { kind: 'test.note', path: 'artifacts/note.md' };
const World = defineDoc<{ seen: string | null }>({ kind: 'acme.world', version: 1, scope: 'conversation', history: 'latest', fork: 'current', initial: () => ({ seen: null }), checkpointWhen: () => false });
const submit = defineTool({
  name: 'submit_note',
  description: 'Seal and score the note',
  parameters: Type.Object({}),
  replay: 'safe',
  execute: async (_args, api, ctx) => {
    await api.commit(async (tx) => { (await tx.doc(World, api.conversationId)).seen = 'pr head abc'; }, ctx);
    const sealed = await seal(api, note, ctx);
    await attachVerdict(api, note.kind, sealed.version, { score: 0.9, passed: true, limitReached: false }, ctx);
    return { output: [{ type: 'text', text: `sealed v${sealed.version}` }] };
  },
});

async function recordedSession() {
  const dir = await mkdtemp(join(tmpdir(), 'quack-eval-test-'));
  dirs.push(dir);
  const faux = fauxProvider({ models: [{ id: 'w' }] });
  faux.setResponses([
    fauxAssistantMessage(fauxToolCall('write', { path: 'artifacts/note.md', content: 'v1 note' }), { stopReason: 'toolUse' }),
    fauxAssistantMessage(fauxToolCall('submit_note', {}), { stopReason: 'toolUse' }),
    fauxAssistantMessage('Noted.'),
  ]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const extensions = [CodingTools, artifactsExtension([note]), defineExtension({ name: 'notes', tools: [submit] })];
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
  await session.submit('Write the note.', 'r-1', true);
  await session.close();
  return dir;
}

test('a recorded session reads back without resuming it, and becomes a dataset item with its world pinned', async () => {
  const record = await readSession(await recordedSession());
  assert.deepEqual(record.inputs, ['Write the note.']);
  assert.deepEqual(record.outcome, { status: 'done', answer: 'Noted.' });
  assert.equal(record.turns, 3);
  assert.deepEqual(record.artifacts['test.note'].map((v) => [v.version, v.content, v.verdict]), [[1, 'v1 note', { score: 0.9, passed: true, limitReached: false }]]);

  const item = itemFrom(record, { agent: 'noter', label: 'bad' });
  assert.deepEqual(item.input, { inputs: ['Write the note.'], pins: { 'acme.world': { seen: 'pr head abc' } } }, 'extension docs are pinned; pi and quack bookkeeping are not');
  assert.deepEqual((item.expectedOutput as any).artifacts['test.note'], { version: 1, content: 'v1 note', score: 0.9, passed: true, limitReached: false });
  assert.equal((item.metadata as any).label, 'bad');
  assert.equal(item.id, itemFrom(record, { agent: 'noter' }).id, 'the id follows the inputs, so re-adding a session replaces its item');
});

async function fakeLangfuse(answer: (method: string, path: string) => [number, unknown] | undefined = () => undefined) {
  const calls: Array<{ method: string; path: string; auth: string | undefined; body: any }> = [];
  const server = createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    calls.push({ method: req.method!, path: req.url!, auth: req.headers.authorization, body: raw ? JSON.parse(raw) : undefined });
    const [status, body] = answer(req.method!, req.url!) ?? [200, {}];
    res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
  }).listen(0, '127.0.0.1');
  await new Promise((r) => server.once('listening', r));
  after(() => server.close());
  return { calls, client: langfuse({ baseUrl: `http://127.0.0.1:${(server.address() as AddressInfo).port}/`, publicKey: 'pk', secretKey: 'sk' }) };
}

test('datasets are created once, items upsert, and every page of items is read', async () => {
  const { calls, client } = await fakeLangfuse((method, path) => {
    if (method === 'GET' && path.startsWith('/api/public/v2/datasets/')) return [404, {}];
    if (path.includes('page=1')) return [200, { data: [{ id: 'a', input: 1 }], meta: { totalPages: 2 } }];
    if (path.includes('page=2')) return [200, { data: [{ id: 'b', input: 2 }], meta: { totalPages: 2 } }];
  });
  await client.ensureDataset('reviewer bench');
  await client.upsertItem('reviewer bench', { id: 'a', input: 1 });
  assert.deepEqual((await client.items('reviewer bench')).map((i) => i.id), ['a', 'b']);
  assert.deepEqual(calls.slice(0, 3).map((c) => `${c.method} ${c.path}`), ['GET /api/public/v2/datasets/reviewer%20bench', 'POST /api/public/v2/datasets', 'POST /api/public/dataset-items']);
  assert.equal(calls[0].auth, `Basic ${Buffer.from('pk:sk').toString('base64')}`);
  assert.deepEqual(calls[2].body, { datasetName: 'reviewer bench', id: 'a', input: 1 });
});

const result: ReplayResult = {
  item: { id: 'item-1', input: { inputs: ['x'], pins: {} } },
  seconds: 42,
  record: { inputs: ['x'], outcome: { status: 'done', answer: 'ok' }, turns: 7, usage: { input: 100, output: 20, cacheRead: 900 }, docs: {},
    artifacts: { 'github.review': [{ kind: 'github.review', version: 1, hash: 'h', content: 'v1', sealedAt: 0, verdict: { score: 0.6, passed: false } }, { kind: 'github.review', version: 2, hash: 'h2', content: 'v2', sealedAt: 0, verdict: { score: 0.95, passed: true } }] } },
  captured: [{ event: 'APPROVE' }],
};

test('a replay is published as a trace with its scores and linked into the run', async () => {
  const { calls, client } = await fakeLangfuse();
  const scores = await publish(client, 'candidate-a', result);
  assert.deepEqual(scores, { checker_score: 0.95, passed: 1, revisions: 2, turns: 7, seconds: 42, input_tokens: 1000, output_tokens: 20, delivered: 1, answered: 1 });
  const [otel, ingest, link] = calls;
  assert.equal(otel.path, '/api/public/otel/v1/traces', 'the trace goes over OpenTelemetry, the only trace path Langfuse v4 events_only accepts');
  const span = otel.body.resourceSpans[0].scopeSpans[0].spans[0];
  assert.match(span.traceId, /^[0-9a-f]{32}$/);
  const attrs = Object.fromEntries(span.attributes.map((a: any) => [a.key, a.value.stringValue]));
  assert.equal(JSON.parse(attrs['langfuse.trace.output']).artifacts['github.review'], 'v2', 'the final version is the output');
  assert.equal(ingest.body.batch.some((e: any) => e.type === 'trace-create'), false);
  assert.deepEqual(ingest.body.batch.map((e: any) => [e.body.name, e.body.value, e.body.traceId === span.traceId]).slice(0, 2), [['checker_score', 0.95, true], ['passed', 1, true]]);
  assert.deepEqual(link.body, { runName: 'candidate-a', datasetItemId: 'item-1', traceId: span.traceId });
});

test('an ingestion that partly fails is an error, not a silent gap in the experiment', async () => {
  const { client } = await fakeLangfuse((_m, path) => (path === '/api/public/ingestion' ? [207, { successes: [], errors: [{ id: 'x-passed', status: 400, message: 'bad body' }] }] : undefined));
  await assert.rejects(publish(client, 'run', result), /langfuse ingest: 1 of 9 events failed; first x-passed: 400 bad body/);
});

test('a runtime that exits at startup ends the replay at once with its error, not at the deadline', async () => {
  const started = Date.now();
  const r = await replay({ id: 'x', input: { inputs: ['go'], pins: {} } }, { deployment: '/nonexistent/deployment.json', agent: '/nonexistent/agent.json' }, {});
  assert.match(r.error ?? '', /^runtime exited with 1: .*nonexistent\/deployment\.json/);
  assert.ok(Date.now() - started < 15_000, `took ${Date.now() - started} ms`);
  assert.deepEqual(r.record.outcome, { status: 'pending' });
});

test('a runtime killed by a signal also ends the replay at once', async () => {
  const started = Date.now();
  const r = await replay({ id: 'x', input: { inputs: ['go'], pins: {} } }, { deployment: '/dev/null', agent: '/dev/null', env: { NODE_OPTIONS: '--import=data:text/javascript,process.kill(process.pid,9)' } }, {});
  assert.match(r.error ?? '', /^runtime exited with SIGKILL/);
  assert.ok(Date.now() - started < 15_000, `took ${Date.now() - started} ms`);
  assert.deepEqual(r.record.outcome, { status: 'pending' });
});

test('a labelled case scores whether the replay blocked it, so a passing review that misses a known defect still counts as a miss', () => {
  const withLabel = (label: string, content: string): ReplayResult => ({ ...result, item: { ...result.item, metadata: { label } }, record: { ...result.record, artifacts: { 'github.review': [{ ...result.record.artifacts['github.review'][1], content }] } } });
  const blocks = (f: Record<string, string>) => f['github.review'].includes('request_changes');
  assert.equal(scoresOf(withLabel('bad', 'request_changes'), blocks).caught, 1);
  assert.equal(scoresOf(withLabel('bad', 'approve'), blocks).caught, 0);
  assert.equal(scoresOf(withLabel('good', 'request_changes'), blocks).false_block, 1);
  assert.equal(scoresOf(withLabel('good', 'approve'), blocks).false_block, 0);
  assert.equal('caught' in scoresOf(result, blocks), false, 'an unlabelled case has no catch score');
});
