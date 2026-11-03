import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { num, obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { openSession } from '@quack/session';
import { catalog } from '../src/catalog.ts';

const root = await mkdtemp(join(tmpdir(), 'quack-codemode-'));
after(() => rm(root, { recursive: true, force: true }));

const probe = (name: string, seen: string[]) =>
  defineTool({ name, description: `Looks up ${name}`, parameters: obj({ id: num() }, { open: true }), execute: async (a) => (seen.push(`${name}:${a.id}`), { output: [{ type: 'text', text: `${name} ${a.id}` }] }) });

async function run(dir: string, codemode: object | undefined, script: string) {
  const seen: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  let offered = '';
  let transcript = '';
  faux.setResponses([
    (c) => ((offered = JSON.stringify(c.messages)), fauxAssistantMessage(fauxToolCall('codemode', { code: script }), { stopReason: 'toolUse' })),
    (c) => ((transcript = JSON.stringify(c.messages)), fauxAssistantMessage('done')),
  ]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const extensions = [defineExtension({ name: 'probes', tools: [probe('alpha', seen), probe('beta', seen)] }), ...(codemode ? await catalog.codemode(codemode) : [])];
  const session = await openSession({ dir: join(root, dir), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
  await session.submit('go', dir, true);
  await session.close();
  return { seen, offered, transcript };
}

const script = 'const a = await tools.alpha({ id: 1 });\nconst b = await tools.beta({ id: 2 });\ntext(a + " " + b);';

test('a script calls the session tools as nested calls and only its output reaches the model', async () => {
  const { seen, offered, transcript } = await run('open', {}, script);
  assert.match(offered, /"name":"codemode"/);
  assert.deepEqual(seen, ['alpha:1', 'beta:2']);
  assert.match(transcript, /alpha 1/);
  assert.match(transcript, /beta 2/);
  assert.match(transcript, /"calls":\[\{"id":1,"name":"alpha".*"id":2,"name":"beta"/, 'the result records both nested calls');
});

test('a tools allowlist leaves other tools out of the script', async () => {
  const { seen, transcript } = await run('narrowed', { tools: ['alpha'] }, script);
  assert.deepEqual(seen, ['alpha:1']);
  assert.doesNotMatch(transcript, /beta 2/);
  assert.doesNotMatch(transcript, /beta\(args/, 'the prompt section lists only the allowed tools');
});

test('an agent that does not select codemode is not offered the tool', async () => {
  const { offered } = await run('plain', undefined, script);
  assert.doesNotMatch(offered, /"name":"codemode"/);
  assert.match(offered, /"name":"alpha"/);
});
