import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { decide, sessionModel } from '@quack/sdk';
import { openSession } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-decide-'));
after(() => rm(dir, { recursive: true, force: true }));

const pickFlex = defineTool({
  name: 'pick_flex',
  description: 'Choose the flex starter',
  parameters: Type.Object({}),
  replay: 'safe',
  execute: async (_args, api, ctx) => {
    const d = await decide(api, { name: 'flex', question: 'Who starts at flex?', options: ['Pollard', 'Stevenson'] }, [sessionModel], ctx);
    return { output: [{ type: 'text', text: d ? `${d.choice} via ${d.resolver}` : 'undecided' }] };
  },
});

test('the default resolver answers on a tool-free fork that sees the whole conversation', async () => {
  let forkRequest: { messages: unknown[]; tools?: unknown[] } | undefined;
  let toolResult = '';
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([
    fauxAssistantMessage(fauxToolCall('pick_flex', {}), { stopReason: 'toolUse' }),
    (context) => {
      forkRequest = context as never;
      return fauxAssistantMessage('{"choice": "Pollard", "confidence": 0.7, "reason": "indoor game"}');
    },
    (context) => {
      toolResult = JSON.stringify(context.messages.at(-1));
      return fauxAssistantMessage('Start Pollard.');
    },
  ]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({
    dir,
    models,
    agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'sleeper-test', tools: [pickFlex] })] },
  });

  const done = await session.submit('Stevenson has wind, Pollard plays indoors. Set my flex.', 'flex-1', true);
  assert.equal(done.status === 'done' && done.answer, 'Start Pollard.');
  assert.match(toolResult, /Pollard via session/);

  const sent = JSON.stringify(forkRequest!.messages);
  assert.match(sent, /Pollard plays indoors/, 'the fork sees the original conversation');
  assert.match(sent, /Decision: Who starts at flex\?/);
  const roles = (forkRequest!.messages as Array<{ role: string }>).map((msg) => msg.role);
  assert.ok(!roles.includes('assistant') && !roles.includes('toolResult'), 'the fork ends before the in-flight tool round');
  assert.equal(forkRequest!.tools?.length ?? 0, 0, 'the fork offers no tools');
  await session.close();
});
