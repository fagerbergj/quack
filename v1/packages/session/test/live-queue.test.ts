import assert from 'node:assert/strict';
import { mkdtemp } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { decide, person } from '@quack/sdk';
import { createSessionServer, openSession } from '../src/index.ts';
import { nextFrame, runEnded } from './ws-helpers.ts';

test('follow-ups queued during a run all reach the model together in the next turn', async (t) => {
  const escalate = defineTool({
    name: 'escalate', description: 'ask a person', parameters: obj({}, { open: true }), replay: 'safe',
    execute: async (_a, api, c) => {
      const d = await decide(api, { name: 'flex', question: 'Who starts?', options: ['Pollard'] }, [person()], c);
      return { output: [{ type: 'text', text: d?.choice ?? 'none' }] };
    },
  });
  // Each model call records the user messages it was sent.
  const calls: string[][] = [];
  const answer = (text: string) => (context: { messages: Array<{ role: string; content: unknown }> }) => {
    calls.push(context.messages.filter((x) => x.role === 'user').map((x) => (typeof x.content === 'string' ? x.content : JSON.stringify(x.content))));
    return text === 'tool' ? fauxAssistantMessage(fauxToolCall('escalate', {}), { stopReason: 'toolUse' }) : fauxAssistantMessage(text);
  };
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(['tool', 'A', 'B', 'C', 'D'].map(answer) as never);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: await mkdtemp(join(tmpdir(), 'quack-live-')), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'demo', tools: [escalate] })] } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const ws = new WebSocket(`${base.replace('http', 'ws')}/ws`);
  t.after(async () => {
    ws.close();
    await server.shutdown();
    await session.close();
  });
  const asked = nextFrame(ws, (f) => f.type === 'standing' && f.questions.length === 1);
  const invoke = (input: string, wait = false) => fetch(`${base}/invocations`, { method: 'POST', body: JSON.stringify({ input, wait, whenBusy: 'followUp' }) }).then((r) => r.json());
  await invoke('go');
  const [question] = (await asked).questions;
  assert.deepEqual([(await invoke('one')).status, (await invoke('two')).status], ['queued', 'queued']);
  const last = invoke('three', true);
  const queuedAll = nextFrame(ws, (f) => f.type === 'queued' && f.items.length === 3);
  await queuedAll;
  await fetch(`${base}/questions/${encodeURIComponent(question.id)}`, { method: 'POST', body: JSON.stringify({ choice: 'Pollard' }) });
  assert.equal((await last).status, 'done');
  const turn = calls.find((users) => users.some((u) => u.includes('one')));
  assert.ok(turn?.some((u) => u.includes('two')) && turn.some((u) => u.includes('three')), `the turn that took "one" also took the rest: ${JSON.stringify(calls)}`);
  assert.equal(calls.length, 3, 'the tool call, its answer, and one turn for all three follow-ups');
  assert.deepEqual(await session.queued(), []);
});

test('a follow-up sent during a run is queued on the server: a reload sees it, the cross withdraws it, Send now steers it', async () => {
  const escalate = defineTool({
    name: 'escalate', description: 'ask a person', parameters: obj({}, { open: true }), replay: 'safe',
    execute: async (_a, api, c) => {
      const d = await decide(api, { name: 'flex', question: 'Who starts?', options: ['Pollard'] }, [person()], c);
      return { output: [{ type: 'text', text: d?.choice ?? 'none' }] };
    },
  });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage(fauxToolCall('escalate', {}), { stopReason: 'toolUse' }), fauxAssistantMessage('A'), fauxAssistantMessage('B'), fauxAssistantMessage('C')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: await mkdtemp(join(tmpdir(), 'quack-live-')), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'demo', tools: [escalate] })] } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const port = (server.address() as AddressInfo).port;
  const open = () => new WebSocket(`ws://127.0.0.1:${port}/ws`);
  const ws = open();
  const asked = nextFrame(ws, (f) => f.type === 'standing' && f.questions.length === 1);
  await fetch(`http://127.0.0.1:${port}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'go' }) });
  const [question] = (await asked).questions;

  const queued = (w: WebSocket, texts: string[]) => nextFrame(w, (f) => f.type === 'queued' && JSON.stringify(f.items.map((i: any) => i.text)) === JSON.stringify(texts));
  const first = queued(ws, ['later']);
  ws.send(JSON.stringify({ type: 'input', input: 'later', whenBusy: 'followUp' }));
  const [item] = (await first).items;

  // A reload is a new socket: its first frames already carry the queue.
  const again = open();
  assert.deepEqual((await queued(again, ['later'])).items, [item]);
  again.close();

  const gone = queued(ws, []);
  ws.send(JSON.stringify({ type: 'withdraw', id: item.id }));
  await gone;

  const second = queued(ws, ['now please']);
  ws.send(JSON.stringify({ type: 'input', input: 'now please', whenBusy: 'followUp' }));
  const [promoted] = (await second).items;
  const steered = queued(ws, []);
  ws.send(JSON.stringify({ type: 'steer', id: promoted.id }));
  await steered;

  const ended = nextFrame(ws, runEnded());
  await fetch(`http://127.0.0.1:${port}/questions/${encodeURIComponent(question.id)}`, { method: 'POST', body: JSON.stringify({ choice: 'Pollard', by: 'jf' }) });
  await ended;
  const reloaded = open();
  const text = JSON.stringify((await nextFrame(reloaded, (f) => f.type === 'view' && f.items)).items);
  reloaded.close();
  assert.match(text, /now please/);
  assert.doesNotMatch(text, /later/);

  ws.close();
  await new Promise((r) => server.close(r));
  await session.close();
});
