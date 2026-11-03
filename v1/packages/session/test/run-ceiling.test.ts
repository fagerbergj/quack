import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { num, obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { failedRows, Failures } from '@quack/sdk';
import { openSession, parseDefinition, parseDeployment, type Catalog } from '../src/index.ts';

const root = await mkdtemp(join(tmpdir(), 'quack-ceiling-'));
after(() => rm(root, { recursive: true, force: true }));

// Every call differs and so does every result, so the loop guard never fires.
const step = (n: number) => (_c: unknown) => fauxAssistantMessage(fauxToolCall('probe', { n }), { stopReason: 'toolUse' });

async function open(name: string, turnCeiling: number | undefined, steps: FauxResponseStep[]) {
  const probe = defineTool({ name: 'probe', description: 'Looks something up', parameters: obj({ n: num() }, { open: true }), execute: async (a) => ({ output: [{ type: 'text', text: `result ${a.n}` }] }) });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(steps);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: join(root, name), models, agent: { model: { provider: m.provider, modelId: m.id }, turnCeiling, extensions: [defineExtension({ name: 'probe', tools: [probe] })] } });
  return { session, faux };
}

const failures = async (session: Awaited<ReturnType<typeof open>>['session']) => failedRows(await session.harness.snapshot(Failures, session.root.id, BACKGROUND_CONTEXT));

test('a run past its turn ceiling is ended and recorded as a failure the session shows', async () => {
  const { session, faux } = await open('over', 5, [...Array.from({ length: 50 }, (_, i) => step(i)), fauxAssistantMessage('never')]);
  const done = await session.submit('go', 'over', true);
  assert.notEqual(done.status, 'done', 'the run did not finish normally');
  assert.ok(faux.state.callCount <= 7, `stopped near the ceiling, not after ${faux.state.callCount} requests`);
  const [row] = await failures(session);
  assert.equal(row.step, 'runaway');
  assert.match(row.error, /more than 5 model turns/);
  await session.close();
});

test('a run within the ceiling is untouched, and a later run clears an earlier ceiling failure', async () => {
  const { session, faux } = await open('within', 3, [...Array.from({ length: 10 }, (_, i) => step(i))]);
  await session.submit('go', 'within-1', true);
  assert.equal((await failures(session)).length, 1);
  faux.setResponses([step(100), fauxAssistantMessage('Done.')]);
  const again = await session.submit('again', 'within-2', true);
  assert.equal(again.status === 'done' && again.answer, 'Done.');
  assert.deepEqual(await failures(session), []);
  await session.close();
});

test('turnCeiling is a positive integer in an agent definition', () => {
  const deployment = parseDeployment({ providers: { local: { baseUrl: 'http://x/v1', models: { m: { contextWindow: 8192, maxTokens: 1024 } } } } });
  const catalog: Catalog = { coding: () => [] };
  const base = { name: 'r', version: 1, model: 'local/m', extensions: {} };
  assert.equal(parseDefinition({ ...base, turnCeiling: 50 }, deployment, catalog).turnCeiling, 50);
  for (const bad of [0, -1, 1.5, '300']) assert.throws(() => parseDefinition({ ...base, turnCeiling: bad }, deployment, catalog), /turnCeiling must be a positive integer/);
});
