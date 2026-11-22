import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { createSessionServer, openSession } from '../src/index.ts';

type Ctx = { messages: Array<{ role: string; content: unknown }> };
const userTexts = (c: Ctx) => c.messages.filter((m) => m.role === 'user').map((m) => (typeof m.content === 'string' ? m.content : JSON.stringify(m.content)));

test('an HTTP steer joins the running run at its next turn instead of queueing behind it', async (t) => {
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  let started!: () => void;
  const running = new Promise<void>((r) => (started = r));
  const hold = defineTool({
    name: 'hold', description: 'hold until released', parameters: obj({}, { open: true }),
    execute: async () => (started(), await held, { output: [{ type: 'text', text: 'held' }] }),
  });
  const turns: string[][] = [];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  // Two replies only: a steer queued behind the run as a follow-up would need a third.
  faux.setResponses([() => fauxAssistantMessage(fauxToolCall('hold', {}), { stopReason: 'toolUse' }), (c: Ctx) => (turns.push(userTexts(c)), fauxAssistantMessage('checked both'))] as never);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const dir = await mkdtemp(join(tmpdir(), 'quack-http-steer-'));
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'demo', tools: [hold] })] } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  t.after(async () => { release(); await server.shutdown(); await session.close(); await rm(dir, { recursive: true, force: true }); });
  const invoke = (body: object) => fetch(`http://127.0.0.1:${(server.address() as AddressInfo).port}/invocations`, { method: 'POST', body: JSON.stringify(body) }).then(async (r) => ({ status: r.status, body: await r.json() }));

  await invoke({ input: 'check the code' });
  await running;
  assert.equal((await invoke({ input: 'x', whenBusy: 'later' })).status, 400);
  const steer = await invoke({ input: 'also check the docs', whenBusy: 'steer' });
  assert.equal(steer.status, 202, JSON.stringify(steer.body));
  release();
  const settled = await (await session.harness.submission(steer.body.submissionId, ctx))!.wait(ctx);
  assert.equal(settled.status, 'done');
  assert.equal(turns.length, 1);
  assert.ok(turns[0]!.some((u) => u.includes('also check the docs')), `the turn after the tool saw the steer: ${JSON.stringify(turns)}`);
});
