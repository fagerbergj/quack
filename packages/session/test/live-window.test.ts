import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxProvider } from '@earendil-works/pi-ai';
import { openSession } from '../src/runtime.ts';
import { createSessionServer } from '../src/server.ts';
import { nextFrame } from './ws-helpers.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-live-window-'));
after(() => rm(dir, { recursive: true, force: true }));

test('the first view holds the newest window and an earlier request pages the rest, so no entry is lost', async () => {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id } } });
  await session.root.commit(async (tx) => {
    for (let i = 1; i <= 120; i++) await tx.appendEntry(session.root.id, { kind: 'pi.user', model: [{ role: 'user', content: `n${i}`, timestamp: i }] } as never);
  }, BACKGROUND_CONTEXT);
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const ws = new WebSocket(`ws://127.0.0.1:${(server.address() as AddressInfo).port}/ws`);

  const first = await nextFrame(ws, (f) => f.type === 'view');
  const texts = (frame: any): string[] => frame.items.flatMap((i: any) => (i.kind === 'entry' ? [i.entry.model[0].content] : []));
  assert.equal(first.items[0].kind, 'earlier');
  assert.equal(texts(first).at(-1), 'n120');
  assert.ok(texts(first).length <= 100);

  const paged = nextFrame(ws, (f) => f.type === 'view' && f.items !== undefined && texts(f).length > texts(first).length);
  ws.send(JSON.stringify({ type: 'earlier' }));
  const more = texts(await paged);
  assert.equal(more.at(-1), 'n120');
  assert.ok(more.length > texts(first).length);

  ws.close();
  await new Promise((r) => server.close(r));
  await session.close();
});
