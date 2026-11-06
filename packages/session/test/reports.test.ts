import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { defineExtension, GenerationTask, hook } from '@earendil-works/pi-durable';
import { openSession } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-turns-'));
after(() => rm(dir, { recursive: true, force: true }));

test('a throwing hook yields one report line naming its extension', async () => {
  const lines: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage('hi')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const bad = defineExtension({
    name: 'bad',
    hooks: [hook(GenerationTask, { afterResponse: () => { throw new Error('boom'); } })],
  });
  const reportDir = await mkdtemp(join(tmpdir(), 'quack-report-'));
  const session = await openSession({ dir: reportDir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [bad] }, log: (l) => lines.push(l) });
  await session.submit('hello', 'r-1', true);
  assert.deepEqual(lines, ['session: extension_error task=pi.generation extension=bad: boom']);
  await session.close();
  await rm(reportDir, { recursive: true, force: true });
});
