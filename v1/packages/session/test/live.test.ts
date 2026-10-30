import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { createSessionServer, openSession } from '../src/index.ts';
import { nextFrame, runEnded } from './ws-helpers.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-live-'));
after(() => rm(dir, { recursive: true, force: true }));

test('streams a snapshot, then the run, and accepts input over the socket', async () => {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage('Paris')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id } } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));

  const ws = new WebSocket(`ws://127.0.0.1:${(server.address() as AddressInfo).port}/ws`);
  const snapshot = await nextFrame(ws, () => true);
  assert.equal(snapshot.type, 'snapshot');

  const bad = nextFrame(ws, (f) => f.type === 'error');
  ws.send(JSON.stringify({ type: 'input', input: '' }));
  assert.match((await bad).error, /non-empty/);

  const ended = nextFrame(ws, runEnded);
  const accepted = nextFrame(ws, (f) => f.type === 'accepted');
  ws.send(JSON.stringify({ type: 'input', input: 'Capital of France?', requestId: 'live-1' }));
  assert.equal((await accepted).submissionId !== undefined, true);
  assert.match(JSON.stringify((await ended).events), /Paris/);

  ws.close();
  await new Promise((r) => server.close(r));
  await session.close();
});
