import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { obj, str } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { Artifacts, decide, gated, person } from '@quack/sdk';
import { createSessionServer, openSession } from '../src/index.ts';
import { nextFrame, runEnded } from './ws-helpers.ts';

test('shutdown closes the server while a page holds its live socket open', async () => {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const own = await mkdtemp(join(tmpdir(), 'quack-live-'));
  after(() => rm(own, { recursive: true, force: true }));
  const session = await openSession({ dir: own, models, agent: { model: { provider: m.provider, modelId: m.id } } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const ws = new WebSocket(`ws://127.0.0.1:${(server.address() as AddressInfo).port}/ws`);
  assert.equal((await nextFrame(ws, () => true)).type, 'view');
  const closed = new Promise((r) => ws.addEventListener('close', r));
  await server.shutdown();
  await closed;
  await session.close();
});

const dir = await mkdtemp(join(tmpdir(), 'quack-live-'));
after(() => rm(dir, { recursive: true, force: true }));

test('streams the conversation view, then the run, and accepts input over the socket', async () => {
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
  assert.equal(snapshot.type, 'view');

  const bad = nextFrame(ws, (f) => f.type === 'error');
  ws.send(JSON.stringify({ type: 'input', input: '' }));
  assert.match((await bad).error, /non-empty/);

  const ended = nextFrame(ws, runEnded());
  const accepted = nextFrame(ws, (f) => f.type === 'accepted');
  ws.send(JSON.stringify({ type: 'input', input: 'Capital of France?', requestId: 'live-1' }));
  assert.equal((await accepted).submissionId !== undefined, true);
  assert.match(JSON.stringify((await ended).items), /Paris/);

  ws.close();
  await new Promise((r) => server.close(r));
  await session.close();
});

test('a failed submit is an error frame, and the session keeps streaming', async () => {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage('still here')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: await mkdtemp(join(tmpdir(), 'quack-live-')), models, agent: { model: { provider: m.provider, modelId: m.id } } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const ws = new WebSocket(`ws://127.0.0.1:${(server.address() as AddressInfo).port}/ws`);
  await nextFrame(ws, () => true);

  const submit = session.submit;
  session.submit = async () => { throw new Error('storage unavailable'); };
  const failed = nextFrame(ws, (f) => f.type === 'error');
  ws.send(JSON.stringify({ type: 'input', input: 'hello' }));
  assert.equal((await failed).error, 'storage unavailable');

  session.submit = submit;
  const tooLong = nextFrame(ws, (f) => f.type === 'error');
  ws.send(JSON.stringify({ type: 'input', input: 'x'.repeat((1 << 20) + 1) }));
  assert.equal((await tooLong).error, 'input is too long');

  const ended = nextFrame(ws, runEnded());
  ws.send(JSON.stringify({ type: 'input', input: 'are you there?', requestId: 'live-2' }));
  assert.match(JSON.stringify((await ended).items), /still here/);
  ws.close();
  await new Promise((r) => server.close(r));
  await session.close();
});

test('an open question rides the socket as a standing frame and clears when answered, with no polling', async () => {
  const escalate = defineTool({
    name: 'escalate', description: 'ask a person', parameters: obj({}, { open: true }), replay: 'safe',
    execute: async (_a, api, c) => {
      const d = await decide(api, { name: 'flex', question: 'Who starts?', options: ['Pollard', 'Stevenson'] }, [person()], c);
      return { output: [{ type: 'text', text: d?.choice ?? 'none' }] };
    },
  });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage(fauxToolCall('escalate', {}), { stopReason: 'toolUse' }), fauxAssistantMessage('Done.')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: await mkdtemp(join(tmpdir(), 'quack-live-')), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'demo', tools: [escalate] })] } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const port = (server.address() as AddressInfo).port;
  const ws = new WebSocket(`ws://127.0.0.1:${port}/ws`);
  const quiet = nextFrame(ws, (f) => f.type === 'standing');
  assert.deepEqual(await quiet, { type: 'standing', questions: [], failures: [], approvals: [] });

  const asked = nextFrame(ws, (f) => f.type === 'standing' && f.questions.length === 1);
  await fetch(`http://127.0.0.1:${port}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'go' }) });
  const [question] = (await asked).questions;
  assert.deepEqual([question.question, question.options], ['Who starts?', ['Pollard', 'Stevenson']]);

  const cleared = nextFrame(ws, (f) => f.type === 'standing' && f.questions.length === 0);
  const res = await fetch(`http://127.0.0.1:${port}/questions/${encodeURIComponent(question.id)}`, { method: 'POST', body: JSON.stringify({ choice: 'Pollard', by: 'jf' }) });
  assert.equal(res.status, 200);
  await cleared;

  ws.close();
  await new Promise((r) => server.close(r));
  await session.close();
});

test('a gated call\'s approval and each sealed artifact ride the socket the moment they are committed', async () => {
  const post = gated(defineTool({ name: 'post_comment', description: 'post', parameters: obj({ body: str() }, { open: true }), execute: async () => ({ output: [{ type: 'text', text: 'posted' }] }) }));
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage(fauxToolCall('post_comment', { body: 'LGTM' }), { stopReason: 'toolUse' }), fauxAssistantMessage('Done.')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: await mkdtemp(join(tmpdir(), 'quack-live-')), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'demo', tools: [post] })] } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  const port = (server.address() as AddressInfo).port;
  const ws = new WebSocket(`ws://127.0.0.1:${port}/ws`);
  assert.deepEqual(await nextFrame(ws, (f) => f.type === 'artifacts'), { type: 'artifacts', artifacts: [] });

  const asked = nextFrame(ws, (f) => f.type === 'standing' && f.approvals.length === 1);
  await fetch(`http://127.0.0.1:${port}/invocations`, { method: 'POST', body: JSON.stringify({ input: 'go' }) });
  const [approval] = (await asked).approvals;
  assert.deepEqual([approval.tool, approval.args], ['post_comment', { body: 'LGTM' }]);
  const cleared = nextFrame(ws, (f) => f.type === 'standing' && f.approvals.length === 0);
  const res = await fetch(`http://127.0.0.1:${port}/approvals/${encodeURIComponent(approval.id)}`, { method: 'POST', body: JSON.stringify({ decision: 'approve' }) });
  assert.equal(res.status, 200);
  await cleared;

  const version = { kind: 'demo.note', version: 1, hash: 'h', content: 'hi', sealedAt: 1, verdict: null };
  const sealed = nextFrame(ws, (f) => f.type === 'artifacts' && f.artifacts.length === 1);
  await session.root.commit(async (tx) => { (await tx.doc(Artifacts, session.root.id)).kinds['demo.note'] = { versions: [version] }; }, BACKGROUND_CONTEXT);
  assert.deepEqual((await sealed).artifacts, [version]);

  ws.close();
  await new Promise((r) => server.close(r));
  await session.close();
});
