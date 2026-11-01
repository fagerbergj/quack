import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool, UsageDoc } from '@earendil-works/pi-durable';
import { decide, sessionModel } from '@quack/sdk';
import { openSession } from '@quack/session';
import { collectFacts } from '../src/index.ts';

const ctx = BACKGROUND_CONTEXT;
const dir = await mkdtemp(join(tmpdir(), 'quack-cost-'));
after(() => rm(dir, { recursive: true, force: true }));

test('the cost fact includes the spend of a decision fork, not just the root conversation', async () => {
  const ask = defineTool({
    name: 'ask', description: 'ask', parameters: Type.Object({}),
    execute: async (_a, api, c) => {
      const d = await decide(api, { name: 'q', question: 'Ship it?', options: ['yes', 'no'] }, [sessionModel], c);
      return { output: [{ type: 'text', text: d?.choice ?? 'none' }] };
    },
  });
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([
    fauxAssistantMessage(fauxToolCall('ask', {}), { stopReason: 'toolUse' }),
    fauxAssistantMessage('{"choice":"yes","confidence":1,"reason":"ok"}'),
    fauxAssistantMessage('Done.'),
  ]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'ask', tools: [ask] })] } });
  assert.equal((await session.submit('Decide.', 'c1', true)).status, 'done');

  // The faux provider never prices a call, so price tokens here; what is under test is which conversations are summed.
  const priced = (u: { models: Record<string, unknown>; tools: Record<string, unknown> }) =>
    Object.values({ ...u.models, ...u.tools }).reduce<number>((n, x) => n + (x as { totalTokens: number }).totalTokens * 1e-6, 0);
  const withCost = (u: { models: Record<string, unknown>; tools: Record<string, unknown> }) => {
    const price = (m: Record<string, unknown>) => Object.fromEntries(Object.entries(m).map(([k, x]) => [k, { ...(x as object), cost: { total: (x as { totalTokens: number }).totalTokens * 1e-6 } }]));
    return { models: price(u.models), tools: price(u.tools) };
  };
  const rootOnly = priced((await session.harness.snapshot(UsageDoc, session.root.id, ctx))!);
  const harness = { snapshot: (...a: unknown[]) => (session.harness.snapshot as Function)(...a), usage: async (c: typeof ctx) => withCost(await session.harness.usage(c)) } as unknown as typeof session.harness;
  const cost = (await collectFacts(harness, session.root, { key: 'k', agent: 'a', version: 1 }, 0, ctx)).find((f) => f.id === 'cost');
  assert.ok(rootOnly > 0);
  assert.ok((cost?.data.total as number) > rootOnly, `fact total ${cost?.data.total} should exceed root-only ${rootOnly}`);
  await session.close();
});
