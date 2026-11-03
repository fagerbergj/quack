import assert from 'node:assert/strict';
import { copyFile, mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { createSessionServer, openSession } from '../src/index.ts';
import { nextFrame, runEnded } from './ws-helpers.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-old-log-'));
after(() => rm(dir, { recursive: true, force: true }));

// fixtures/pd14-log/session.sqlite was written by pi-durable 1.4.0 (make.mjs beside it): a tool round and its answer, then a run the model failed.
test('a log written by pi-durable 1.4.0 opens, renders its whole transcript, and takes new input', async () => {
  await copyFile(join(import.meta.dirname, 'fixtures/pd14-log/session.sqlite'), join(dir, 'session.sqlite'));
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage('Back again.')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'be brief' } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const ws = new WebSocket(`ws://127.0.0.1:${(server.address() as AddressInfo).port}/ws`);

  const first = await nextFrame(ws, (f) => f.type === 'view');
  const text = JSON.stringify(first.items);
  for (const seen of ['find ducks', '"name":"lookup"', 'found ducks', 'Ducks found.', 'provider is down']) assert.match(text, new RegExp(seen));
  assert.equal(first.live.run, undefined);
  assert.ok(Object.keys(first.usage.models).length > 0, 'the log keeps its spend');

  const ended = nextFrame(ws, runEnded());
  ws.send(JSON.stringify({ type: 'input', input: 'still there?', requestId: 'old-1' }));
  assert.match(JSON.stringify((await ended).items), /Back again\./);

  ws.close();
  await new Promise((r) => server.close(r));
  await session.close();
});
