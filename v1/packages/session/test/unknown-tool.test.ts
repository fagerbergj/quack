import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { num, obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { openSession } from '../src/index.ts';

const root = await mkdtemp(join(tmpdir(), 'quack-unknown-'));
after(() => rm(root, { recursive: true, force: true }));

test('a call to a tool the agent lacks gets a result naming the tools it has', async () => {
  const mine = defineTool({ name: 'checkout_task', description: 'x', parameters: obj({ id: num() }, { open: true }), execute: async () => ({ output: [{ type: 'text', text: 'ok' }] }) });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  let seen = '';
  faux.setResponses([fauxAssistantMessage(fauxToolCall('checkout_pr', { id: 1 }), { stopReason: 'toolUse' }), (c) => ((seen = JSON.stringify(c.messages)), fauxAssistantMessage('done'))]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: join(root, 's'), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'mine', tools: [mine] })] } });
  await session.submit('go', 'k', true);
  assert.match(seen, /Tool checkout_pr is not available\. Your tools: [^"]*checkout_task/);
  await session.close();
});
