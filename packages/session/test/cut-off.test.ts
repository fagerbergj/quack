import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { openSession } from '../src/index.ts';
import { CUT_OFF_LIMIT } from '../src/cut-off.ts';

const root = await mkdtemp(join(tmpdir(), 'quack-cutoff-'));
after(() => rm(root, { recursive: true, force: true }));

async function run(name: string, replies: ReturnType<typeof fauxAssistantMessage>[]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  let calls = 0;
  let last = '';
  faux.setResponses(replies.map((r) => (c) => (calls++, (last = JSON.stringify(c.messages)), r)));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: join(root, name), models, agent: { model: { provider: m.provider, modelId: m.id } } });
  await session.submit('go', 'k', true);
  await session.close();
  return { calls, last };
}

const cut = () => fauxAssistantMessage('thinking that ran out', { stopReason: 'length' });

test('a reply cut off at the output cap continues the run instead of ending it', async () => {
  const { calls, last } = await run('once', [cut(), fauxAssistantMessage('done')]);
  assert.equal(calls, 2);
  assert.match(last, /hit the output limit and was cut off/);
});

test('cut-offs past the limit end the run', async () => {
  const { calls } = await run('stuck', [...Array.from({ length: CUT_OFF_LIMIT + 1 }, cut), fauxAssistantMessage('never asked')]);
  assert.equal(calls, CUT_OFF_LIMIT + 1);
});

test('a reply that acts resets the streak, so a later cut-off still continues', async () => {
  const acted = fauxAssistantMessage(fauxToolCall('missing_tool', {}), { stopReason: 'toolUse' });
  const { calls } = await run('reset', [...Array.from({ length: CUT_OFF_LIMIT }, cut), acted, cut(), fauxAssistantMessage('done')]);
  assert.equal(calls, CUT_OFF_LIMIT + 3);
});
