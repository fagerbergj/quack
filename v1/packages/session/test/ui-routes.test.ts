import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { attachVerdict, seal, type ArtifactKind } from '@quack/sdk';
import { createSessionServer, openSession } from '../src/index.ts';
import { nextFrame, runEnded } from './ws-helpers.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-ui-routes-'));
after(() => rm(dir, { recursive: true, force: true }));

const note: ArtifactKind = { kind: 'test.note', path: 'note.md' };

async function serve(extension: ReturnType<typeof defineExtension>, ...responses: Parameters<ReturnType<typeof fauxProvider>['setResponses']>[0]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: await mkdtemp(join(dir, 's-')), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, extension] } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  return { session, base, stop: async () => { await new Promise((r) => server.close(r)); await session.close(); } };
}

test('POST /abort ends a run that is waiting on a tool', async () => {
  let started!: () => void;
  const running = new Promise<void>((r) => (started = r));
  const hang = defineTool({
    name: 'hang',
    description: 'waits until aborted',
    parameters: obj({}, { open: true }),
    execute: async (_a, _api, ctx) => {
      started();
      await new Promise((r) => ctx.abortSignal?.addEventListener('abort', r));
      return { output: [{ type: 'text', text: 'stopped' }] };
    },
  });
  const h = await serve(defineExtension({ name: 'hang', tools: [hang] }), fauxAssistantMessage(fauxToolCall('hang', {}), { stopReason: 'toolUse' }), fauxAssistantMessage('unreachable'));
  const ws = new WebSocket(`${h.base.replace('http', 'ws')}/ws`);
  await nextFrame(ws, (f) => f.type === 'view');
  const ended = nextFrame(ws, runEnded());
  await fetch(`${h.base}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'go', requestId: 'a-1' }) });
  await running;
  const res = await fetch(`${h.base}/abort`, { method: 'POST' });
  assert.deepEqual([res.status, await res.json()], [200, { aborted: true }]);
  await ended;
  assert.equal((await h.session.standing()).busy, false);
  ws.close();
  await h.stop();
});

test('GET /artifacts lists sealed versions with their verdicts', async () => {
  const submit = defineTool({
    name: 'submit',
    description: 'seal and check',
    parameters: obj({}, { open: true }),
    replay: 'safe',
    execute: async (_a, api, ctx) => {
      const sealed = await seal(api, note, ctx);
      await attachVerdict(api, note.kind, sealed.version, { passed: false, checks: [] }, ctx);
      return { output: [{ type: 'text', text: 'sealed' }] };
    },
  });
  const h = await serve(
    defineExtension({ name: 'note', tools: [submit] }),
    fauxAssistantMessage(fauxToolCall('write', { path: 'note.md', content: 'hello' }), { stopReason: 'toolUse' }),
    fauxAssistantMessage(fauxToolCall('submit', {}), { stopReason: 'toolUse' }),
    fauxAssistantMessage('done'),
  );
  assert.deepEqual(await (await fetch(`${h.base}/artifacts`)).json(), { artifacts: [] });
  await h.session.submit('write it', 'n-1', true);
  const { artifacts } = (await (await fetch(`${h.base}/artifacts`)).json()) as { artifacts: Array<Record<string, unknown>> };
  assert.equal(artifacts.length, 1);
  assert.deepEqual([artifacts[0].kind, artifacts[0].version, artifacts[0].content, artifacts[0].verdict], ['test.note', 1, 'hello', { passed: false, checks: [] }]);
  await h.stop();
});

test('GET /messages returns the newest person and assistant messages, oldest first, capped by limit', async () => {
  const h = await serve(defineExtension({ name: 'none' }), fauxAssistantMessage('first answer'), fauxAssistantMessage('second answer'));
  await h.session.submit('first question', 'm-1', true);
  await h.session.submit('second question', 'm-2', true);
  const read = async (query: string) => ((await (await fetch(`${h.base}/messages${query}`)).json()) as { messages: Array<{ role: string; text: string }> }).messages;
  assert.deepEqual((await read('')).map((m) => [m.role, m.text]), [['user', 'first question'], ['assistant', 'first answer'], ['user', 'second question'], ['assistant', 'second answer']]);
  assert.deepEqual((await read('?limit=2')).map((m) => m.text), ['second question', 'second answer']);
  await h.stop();
});

test('GET /export returns the transcript with tool calls and results, and the sealed artifacts', async () => {
  const h = await serve(defineExtension({ name: 'none' }), fauxAssistantMessage(fauxToolCall('write', { path: 'a.md', content: 'x' }), { stopReason: 'toolUse' }), fauxAssistantMessage('done'));
  await h.session.submit('write it', 'e-1', true);
  const body = (await (await fetch(`${h.base}/export`)).json()) as { transcript: Array<{ kind: string }>; artifacts: unknown[] };
  assert.deepEqual(body.transcript.map((e) => e.kind), ['pi.user', 'pi.assistant', 'pi.tool-result', 'pi.assistant']);
  assert.deepEqual(body.artifacts, []);
  await h.stop();
});

test('errors are RFC 9457 problem+json, and an oversized body is 413', async () => {
  const { base, stop } = await serve(defineExtension({ name: 'none' }));
  try {
    const missing = await fetch(`${base}/nope`);
    assert.equal(missing.headers.get('content-type'), 'application/problem+json');
    assert.deepEqual(await missing.json(), { type: 'about:blank', title: 'Not Found', status: 404, detail: 'not found', error: 'not found' });
    const bad = await fetch(`${base}/invocations`, { method: 'POST', body: 'not json' });
    assert.equal(bad.status, 400);
    assert.equal(bad.headers.get('content-type'), 'application/problem+json');
    const big = await fetch(`${base}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'x'.repeat(2 * 1024 * 1024) }) });
    assert.equal(big.status, 413);
    assert.equal((await big.json()).detail, 'body too large');
  } finally {
    await stop();
  }
});

test('a query string does not change which route answers', async () => {
  const { base, stop } = await serve(defineExtension({ name: 'none' }));
  try {
    for (const path of ['/approvals?state=pending', '/failures?x=1', '/meta?x=1', '/ping?x=1', '/messages?limit=2']) {
      assert.equal((await fetch(base + path)).status, 200, path);
    }
    assert.equal((await fetch(`${base}/nope?x=1`)).status, 404);
  } finally {
    await stop();
  }
});
