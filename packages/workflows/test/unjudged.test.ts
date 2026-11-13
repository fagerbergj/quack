// Its own file: the judge's gate is session-process state, and this process must never have installed it.
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { obj, str } from '@earendil-works/pi-schema';
import { Artifacts } from '@quack/sdk';
import { openSession } from '@quack/session';
import { defineWorkflow, runWorkflow, workflowExtension } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-wf-unjudged-'));
after(() => rm(dir, { recursive: true, force: true }));

test('without the judge a judged card seals once and goes out unchecked', async () => {
  let asked = 0;
  const sayOk = { name: 'says-ok', version: 1, threshold: 1, criteria: [{ id: 'tone', checks: [{ id: 'says_ok', label: 'Does not say ok', weight: 'gate' as const, code: (a: string) => ({ met: a.includes('ok') ? 1 : 0 }) }] }] };
  const flow = defineWorkflow({
    name: 'judged',
    steps: [{ name: 'card', kind: 'judged', artifact: { kind: 'test.card', path: 'card.md' }, schema: obj({ text: str() }, { open: true }), prompt: () => 'write the text', card: (_c, d) => d as object,
      judge: () => ({ rubric: sayOk, declaration: { scopes: {}, checks: {} }, inputs: async () => ({}) }) }],
  });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([() => (asked++, fauxAssistantMessage('{"text": "fine"}'))]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [workflowExtension([flow])] } });
  const outcome = await runWorkflow(s.harness, s.root, flow, null);
  const versions = (await s.harness.snapshot(Artifacts, s.root.id, ctx))!.kinds['test.card'].versions;
  await s.close();
  assert.equal(asked, 1, 'a card that fails the rubric is not sent back: nothing judged it');
  assert.deepEqual(versions.map((v) => v.verdict ?? null), [null]);
  assert.deepEqual(outcome.status === 'completed' && (outcome.result as { card: unknown }).card, { version: 1, revisions: 0, passed: null, score: null, gatesFailed: [] });
});
