// Writes a session log with pi-durable 1.4.0: a tool round, an answer, a failed run, and a queued follow-up left in the inbox.
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { createRegistry, defineExtension, defineTool, Harness } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';

const dir = process.argv[2];
const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
const models = createModels();
models.setProvider(faux.provider);
const m = faux.getModel();
const lookup = defineTool({
  name: 'lookup', description: 'look up', parameters: Type.Object({ q: Type.String() }), replay: 'safe',
  execute: async (a) => ({ output: [{ type: 'text', text: `found ${a.q}` }] }),
});
const registry = createRegistry();
registry.install(defineExtension({ name: 'old', tools: [lookup] }));
const storage = await openNodeSqliteStorage(`${dir}/session.sqlite`);
const harness = await Harness.open(storage, { models, registry, settings: { afterFailedRun: 'continue' } }, ctx);
const root = await harness.root(ctx, { agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'be brief' } });
harness.resume();
faux.setResponses([
  fauxAssistantMessage(fauxToolCall('lookup', { q: 'ducks' }), { stopReason: 'toolUse' }),
  fauxAssistantMessage('Ducks found.'),
]);
await (await root.submit({ type: 'input', content: 'find ducks' }, ctx)).wait(ctx);
faux.setResponses([fauxAssistantMessage('', { stopReason: 'error', errorMessage: 'provider is down' })]);
await (await root.submit({ type: 'input', content: 'again' }, ctx)).wait(ctx);
await harness.close(ctx);
console.log('written');
