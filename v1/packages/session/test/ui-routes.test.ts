import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
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
    parameters: Type.Object({}),
    execute: async (_a, _api, ctx) => {
      started();
      await new Promise((r) => ctx.abortSignal?.addEventListener('abort', r));
      return { output: [{ type: 'text', text: 'stopped' }] };
    },
  });
  const h = await serve(defineExtension({ name: 'hang', tools: [hang] }), fauxAssistantMessage(fauxToolCall('hang', {}), { stopReason: 'toolUse' }), fauxAssistantMessage('unreachable'));
  const ws = new WebSocket(`${h.base.replace('http', 'ws')}/ws`);
  await nextFrame(ws, (f) => f.type === 'snapshot');
  const ended = nextFrame(ws, runEnded);
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
    parameters: Type.Object({}),
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
