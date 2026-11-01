// A session process whose model calls a gated tool, then reports that it ran, for sleep/wake tests.
import { appendFile } from 'node:fs/promises';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { gated } from '@quack/sdk';
import { createSessionServer, openSession } from '@quack/session';

const extension = defineExtension({
  name: 'gate-fixture',
  tools: [
    gated(
      defineTool({
        name: 'post_comment',
        description: 'Post a comment',
        parameters: Type.Object({}),
        execute: async () => {
          await appendFile(`${process.env.QUACK_SESSION_DIR}/posted`, 'x');
          return { output: [{ type: 'text', text: 'posted' }] };
        },
      }),
    ),
  ],
});

// Each process starts with a fresh script, so the reply depends on the transcript: a tool result means the tool ran.
const faux = fauxProvider({ models: [{ id: 'gate' }] });
faux.setResponses(
  Array.from({ length: 20 }, () => (context: { messages: Array<{ role: string }> }) =>
    context.messages.at(-1)?.role === 'toolResult' ? fauxAssistantMessage('Posted the review.') : fauxAssistantMessage(fauxToolCall('post_comment', {}), { stopReason: 'toolUse' })) as never,
);
const models = createModels();
models.setProvider(faux.provider);
const m = faux.getModel();

const session = await openSession({ dir: process.env.QUACK_SESSION_DIR!, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [extension] } });
const server = createSessionServer(session).listen(Number(process.env.PORT));
process.on('SIGTERM', () => server.close(() => void session.close().then(() => process.exit(0))));
