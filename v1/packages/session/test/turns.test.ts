import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool, GenerationTask, hook } from '@earendil-works/pi-durable';
import { openSession } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-turns-'));
after(() => rm(dir, { recursive: true, force: true }));

test('a run that keeps calling tools is aborted at the turn limit', async () => {
  let calls = 0;
  const ping = defineTool({ name: 'ping', description: 'ping', parameters: Type.Object({}), execute: async () => (calls++, { output: [{ type: 'text', text: 'pong' }] }) });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(Array.from({ length: 20 }, () => fauxAssistantMessage(fauxToolCall('ping', {}), { stopReason: 'toolUse' })));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({
    dir,
    models,
    agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'loop', tools: [ping] })], maxTurns: 3 },
  });
  const result = await session.submit('Keep pinging.', 'loop-1', true);
  assert.equal(result.status, 'unanswered');
  assert.equal(calls, 3, `ran ${calls} tool calls for maxTurns=3`);
  await session.close();
});

test('the turn limit holds across a close and reopen mid-run', async () => {
  const reopenDir = await mkdtemp(join(tmpdir(), 'quack-turns-reopen-'));
  let calls = 0;
  let hang = true;
  let hung!: () => void;
  const stuck = new Promise<void>((r) => (hung = r));
  const ping = defineTool({
    name: 'ping',
    description: 'ping',
    parameters: Type.Object({}),
    execute: async (_args, _api, context) => {
      calls++;
      if (calls === 3 && hang) {
        hung();
        await new Promise((r) => context.abortSignal?.addEventListener('abort', r));
      }
      return { output: [{ type: 'text', text: 'pong' }] };
    },
  });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(Array.from({ length: 30 }, () => fauxAssistantMessage(fauxToolCall('ping', {}), { stopReason: 'toolUse' })));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const agent = { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'loop', tools: [ping] })], maxTurns: 4 };
  const first = await openSession({ dir: reopenDir, models, agent });
  await first.submit('Keep pinging.', 'loop-2');
  await stuck;
  hang = false;
  await first.close();
  const second = await openSession({ dir: reopenDir, models, agent });
  const result = await second.submit('Keep pinging.', 'loop-2', true);
  assert.equal(result.status, 'unanswered');
  assert.equal(faux.state.callCount, 4, `${faux.state.callCount} model turns for maxTurns=4`);
  await second.close();
  await rm(reopenDir, { recursive: true, force: true });
});

test('a throwing hook yields one report line naming its extension', async () => {
  const lines: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage('hi')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const bad = defineExtension({
    name: 'bad',
    hooks: [hook(GenerationTask, { afterResponse: () => { throw new Error('boom'); } })],
  });
  const reportDir = await mkdtemp(join(tmpdir(), 'quack-report-'));
  const session = await openSession({ dir: reportDir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [bad] }, log: (l) => lines.push(l) });
  await session.submit('hello', 'r-1', true);
  assert.deepEqual(lines, ['session: extension_error task=pi.generation extension=bad: boom']);
  await session.close();
  await rm(reportDir, { recursive: true, force: true });
});
