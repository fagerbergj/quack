import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { pendingApprovals, resolveApproval, type Action } from '@quack/sdk';
import { openSession } from '@quack/session';
import { homeSessionExtensions } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

const started: unknown[] = [];
const ping: Action<ReturnType<typeof Type.Object>> = {
  name: 'ping_thing',
  summary: 'Pings a thing',
  params: Type.Object({ thing: Type.String() }),
  async start(params) {
    started.push(params);
    return { session: 'thing:1', title: 'Ping thing' };
  },
};

async function homeSession(...responses: Parameters<ReturnType<typeof fauxProvider>['setResponses']>[0]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-home-'));
  dirs.push(dir);
  const m = faux.getModel();
  return openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: homeSessionExtensions([ping], { distributorUrl: 'http://127.0.0.1:1', fetch, env: {} }) } });
}

const callStart = () => fauxAssistantMessage(fauxToolCall('start', { action: 'ping_thing', params: { thing: 'x' } }), { stopReason: 'toolUse' });

// Waits for the run to stop on the approval; the deadline only matters when the code is broken.
async function pending(session: Awaited<ReturnType<typeof homeSession>>) {
  for (let i = 0; i < 1500; i++) {
    const [item] = await pendingApprovals(session.root, BACKGROUND_CONTEXT);
    if (item) return item;
    await new Promise((r) => setTimeout(r, 10));
  }
  throw new Error('no approval was requested');
}

test('start asks first, showing the summary and filled params, and runs the action only once approved', async () => {
  started.length = 0;
  const session = await homeSession(callStart(), fauxAssistantMessage('Started.'));
  await session.submit('ping x', 'r1');
  const item = await pending(session);
  assert.equal(item.tool, 'start');
  assert.deepEqual(item.args, { action: 'ping_thing', summary: 'Pings a thing: thing x', params: { thing: 'x' } });
  assert.deepEqual(started, [], 'nothing starts before the verdict');

  assert.equal(await resolveApproval(session.root, item.id, { decision: 'approve', by: 'jf' }, BACKGROUND_CONTEXT), true);
  const done = await session.submit('ping x', 'r1', true);
  assert.equal(done.status, 'done');
  assert.deepEqual(started, [{ thing: 'x' }]);
  await session.close();
});

test('a rejected start never runs the action', async () => {
  started.length = 0;
  const session = await homeSession(callStart(), fauxAssistantMessage('Ok, not starting.'));
  await session.submit('ping x', 'r2');
  const item = await pending(session);
  await resolveApproval(session.root, item.id, { decision: 'reject', reason: 'no', by: 'jf' }, BACKGROUND_CONTEXT);
  assert.equal((await session.submit('ping x', 'r2', true)).status, 'done');
  assert.deepEqual(started, []);
  await session.close();
});
