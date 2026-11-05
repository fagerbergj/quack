import { defineExtension, hook, ToolTask, type ConversationId } from '@earendil-works/pi-durable';

/** Names the tools the agent does have when the model calls one it lacks, e.g. a tool copied from another agent's transcript. */
export const unknownToolExtension = (offered: (conversation: ConversationId) => Promise<string[]>) =>
  defineExtension({
    name: 'unknown-tool',
    hooks: [
      hook(ToolTask, {
        async afterTool(call, result, api) {
          if (!result.diagnostics?.some((d) => d.code === 'tool_unavailable')) return undefined;
          const text = `Tool ${call.name} is not available. Your tools: ${(await offered(api.conversationId)).join(', ')}.`;
          return { ...result, output: [{ type: 'text', text }] };
        },
      }),
    ],
  });
