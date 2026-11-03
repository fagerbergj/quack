// A session process whose gated tool records its side effect and then hangs, so a test can kill it mid-execution.
import { appendFileSync } from 'node:fs';
import type { AddressInfo } from 'node:net';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { obj, str } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { gated } from '@quack/sdk';
import { createSessionServer, openSession } from '../../src/index.ts';

const post = defineTool({
  name: 'post_comment',
  description: 'Post a comment to the pull request',
  parameters: obj({ body: str() }, { open: true }),
  execute: async (args) => {
    appendFileSync(process.env.SIDE_EFFECTS!, `${args.body}\n`);
    return new Promise<never>(() => {});
  },
});
const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
faux.setResponses([fauxAssistantMessage(fauxToolCall('post_comment', { body: 'LGTM' }), { stopReason: 'toolUse' })]);
const models = createModels();
models.setProvider(faux.provider);
const m = faux.getModel();
const extensions = [defineExtension({ name: 'test-github', tools: [gated(post)] })];
const session = await openSession({ dir: process.env.QUACK_SESSION_DIR!, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
// Port 0 and an IPC report: a port picked by the test and released before this bind can be taken by another process.
const server = createSessionServer(session).listen(0, () => process.send!({ port: (server.address() as AddressInfo).port }));
