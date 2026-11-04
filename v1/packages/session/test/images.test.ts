import assert from 'node:assert/strict';
import { mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import type { AddressInfo } from 'node:net';
import { createSessionServer, openSession } from '../src/index.ts';
import { nextFrame, runEnded } from './ws-helpers.ts';
import { MAX_IMAGES, parseImages } from '../src/images.ts';

const PIXEL = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==';
const image = { type: 'image' as const, data: PIXEL, mimeType: 'image/png' };

async function open(opts: { sees: boolean }) {
  const faux = fauxProvider({ models: [{ id: 'agent', input: opts.sees ? ['text', 'image'] : ['text'] }] });
  const models = createModels();
  models.setProvider(faux.provider);
  const agent = faux.getModel('agent')!;
  const session = await openSession({
    dir: await mkdtemp(join(tmpdir(), 'quack-images-')),
    models,
    agent: { model: { provider: agent.provider, modelId: agent.id } },
  });
  return { faux, session };
}

test('an image reaches a vision model as image content', async () => {
  const { faux, session } = await open({ sees: true });
  faux.setResponses([(ctx) => {
    const content = (ctx.messages.find((m) => m.role === 'user' && Array.isArray(m.content)) as { content: Array<{ type: string }> }).content;
    return fauxAssistantMessage(content.map((b) => b.type).join(','));
  }]);
  const result = await session.submit('what is this?', 'v1', true, undefined, [image]);
  assert.equal(result.status === 'done' && result.answer, 'text,image');
  await session.close();
});

test('a text-only model rejects the message', async () => {
  const { session } = await open({ sees: false });
  await assert.rejects(session.submit('hi', 'v3', false, undefined, [image]), /cannot see images/);
  await session.close();
});

test('the count and size caps are enforced', () => {
  assert.match(String(parseImages(Array(MAX_IMAGES + 1).fill(image))), /at most 4 images/);
  assert.match(String(parseImages([{ ...image, data: 'A'.repeat(3 * 1024 * 1024 + 1) }])), /over 3 MB/);
  assert.match(String(parseImages([{ data: 'x', mimeType: 'image/gif' }])), /png, jpeg/);
  assert.equal((parseImages([image]) as unknown[]).length, 1);
});

test('the socket takes images with an input, and answers a message with too many by an error frame', async () => {
  const { faux, session } = await open({ sees: true });
  faux.setResponses([fauxAssistantMessage('seen')]);
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const ws = new WebSocket(`ws://127.0.0.1:${(server.address() as AddressInfo).port}/ws`);
  await nextFrame(ws, (f) => f.type === 'view');
  const bad = nextFrame(ws, (f) => f.type === 'error');
  ws.send(JSON.stringify({ type: 'input', requestId: 'r-bad', input: 'x', images: Array(MAX_IMAGES + 1).fill(image) }));
  const failed = await bad;
  assert.match(failed.error, /at most 4 images/);
  assert.equal(failed.requestId, 'r-bad');
  const ended = nextFrame(ws, runEnded());
  const accepted = nextFrame(ws, (f) => f.type === 'accepted');
  ws.send(JSON.stringify({ type: 'input', requestId: 'r-ok', input: '', images: [image] }));
  assert.equal((await accepted).requestId, 'r-ok');
  assert.match(JSON.stringify((await ended).items), /seen/);
  ws.close();
  await new Promise((r) => server.close(r));
  await session.close();
});
