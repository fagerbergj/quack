import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { num, obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { openSession } from '../src/index.ts';
import { LOOP_LIMIT } from '../src/loop-guard.ts';

const root = await mkdtemp(join(tmpdir(), 'quack-loop-'));
after(() => rm(root, { recursive: true, force: true }));

const call = (id: number) => fauxAssistantMessage(fauxToolCall('probe', { id }), { stopReason: 'toolUse' });

async function run(name: string, steps: FauxResponseStep[], answer: (n: number) => string) {
  let executed = 0;
  const probe = defineTool({
    name: 'probe',
    description: 'Looks something up',
    parameters: obj({ id: num() }, { open: true }),
    execute: async () => ({ output: [{ type: 'text', text: answer(++executed) }] }),
  });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(steps);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: join(root, name), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'probe', tools: [probe] })] } });
  const done = await session.submit('go', name, true);
  return { session, faux, done, executed: () => executed };
}

test('the same call returning the same result is stopped after the limit, with the reason in the transcript', async () => {
  let next = '';
  const { session, faux, done, executed } = await run('stuck', [call(1), call(1), call(1), call(1), call(1), fauxAssistantMessage('never')], () => 'unchanged');
  assert.equal(done.status, 'done');
  assert.equal(executed(), LOOP_LIMIT);
  assert.equal(faux.state.callCount, LOOP_LIMIT, 'no model request follows the stopping result');
  faux.setResponses([(c) => ((next = JSON.stringify(c.messages)), fauxAssistantMessage('ok'))]);
  await session.submit('again', 'stuck-2', true);
  assert.match(next, /Stopped: probe was called with the same arguments 3 times/);
  await session.close();
});

test('a call whose result changes is not stopped', async () => {
  const { session, done, executed } = await run('moving', [call(1), call(1), call(1), call(1), fauxAssistantMessage('Done.')], (n) => `state ${n}`);
  assert.equal(done.status === 'done' && done.answer, 'Done.');
  assert.equal(executed(), 4);
  await session.close();
});

test('a loop alternating between two calls is stopped on the call that keeps answering the same', async () => {
  const { session, done, executed } = await run('alternating', [call(1), call(2), call(1), call(2), call(1), call(2), fauxAssistantMessage('never')], () => 'unchanged');
  assert.equal(done.status, 'done');
  assert.equal(executed(), 5);
  await session.close();
});

test('a round of two stuck parallel calls ends the run', async () => {
  const both = () => fauxAssistantMessage([fauxToolCall('probe', { id: 1 }), fauxToolCall('probe', { id: 2 })], { stopReason: 'toolUse' });
  const { session, faux, done, executed } = await run('parallel', [both(), both(), both(), both(), fauxAssistantMessage('never')], () => 'unchanged');
  assert.equal(done.status, 'done');
  assert.equal(faux.state.callCount, LOOP_LIMIT);
  assert.equal(executed(), 2 * LOOP_LIMIT);
  await session.close();
});

test('repeats in separate runs do not add up', async () => {
  const { session, faux, done, executed } = await run('runs', [call(1), call(1), fauxAssistantMessage('first')], () => 'unchanged');
  assert.equal(done.status === 'done' && done.answer, 'first');
  faux.setResponses([call(1), call(1), fauxAssistantMessage('second')]);
  const again = await session.submit('again', 'runs-2', true);
  assert.equal(again.status === 'done' && again.answer, 'second');
  assert.equal(executed(), 4);
  await session.close();
});
