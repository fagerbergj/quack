import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { obj, str } from '@earendil-works/pi-schema';
import { pendingApprovals, resolveApproval, type Action } from '@quack/sdk';
import { openSession } from '@quack/session';
import { homeSessionExtensions } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

const started: unknown[] = [];
const ping: Action<ReturnType<typeof obj>> = {
  name: 'ping_thing',
  summary: 'Pings a thing',
  params: obj({ thing: str() }, { open: true }),
  async start(params) {
    started.push(params);
    return { session: 'thing:1', title: 'Ping thing' };
  },
};

type Responses = Parameters<ReturnType<typeof fauxProvider>['setResponses']>[0];
const homeSession = (...responses: Responses) => homeSessionWith(async () => Response.json({ error: 'no such app' }, { status: 404 }), ...responses);

async function homeSessionWith(doFetch: typeof fetch, ...responses: Responses) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(responses);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-home-'));
  dirs.push(dir);
  const m = faux.getModel();
  return openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: homeSessionExtensions([ping], { distributorUrl: 'http://127.0.0.1:1', fetch: doFetch, env: {} }) } });
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

  assert.equal(await resolveApproval(session.root, item.id, { decision: 'approve', by: { kind: 'person', name: 'jf' } }, BACKGROUND_CONTEXT), true);
  const done = await session.submit('ping x', 'r1', true);
  assert.equal(done.status, 'done');
  assert.deepEqual(started, [{ thing: 'x' }]);
  await session.close();
});

test("an approved start records the session's key with Home's app, which reports its run's end", async () => {
  const posts: string[] = [];
  const recording: typeof fetch = async (input, init) => (posts.push(`${String(input)} ${String(init?.body)}`), Response.json({ ok: true }));
  const session = await homeSessionWith(recording, callStart(), fauxAssistantMessage('Started.'));
  await session.submit('ping x', 'r3');
  const item = await pending(session);
  await resolveApproval(session.root, item.id, { decision: 'approve', by: { kind: 'person', name: 'jf' } }, BACKGROUND_CONTEXT);
  assert.equal((await session.submit('ping x', 'r3', true)).status, 'done');
  assert.deepEqual(posts, ['http://127.0.0.1:1/apps/home/started {"key":"thing:1"}']);
  await session.close();
});

test('a rejected start never runs the action', async () => {
  started.length = 0;
  const session = await homeSession(callStart(), fauxAssistantMessage('Ok, not starting.'));
  await session.submit('ping x', 'r2');
  const item = await pending(session);
  await resolveApproval(session.root, item.id, { decision: 'reject', reason: 'no', by: { kind: 'person', name: 'jf' } }, BACKGROUND_CONTEXT);
  assert.equal((await session.submit('ping x', 'r2', true)).status, 'done');
  assert.deepEqual(started, []);
  await session.close();
});
