import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
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
