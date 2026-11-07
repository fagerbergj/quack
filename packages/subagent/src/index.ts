import { contentText } from '@earendil-works/pi-ai';
import { obj, str } from '@earendil-works/pi-schema';
import { AssistantEntry, configure, defineDoc, defineExtension, defineTool, type Extension } from '@earendil-works/pi-durable';
import { whileWaiting, type ExtensionManifest } from '@quack/sdk';

export type { SubagentArgs, SubagentDetails } from './contract.ts';

/** A runaway guard on the children one session may ever start, not a setting; concurrency is bounded by the model-slot gate. */
export const MAX_CHILDREN = 20;

/** The calls this session has admitted, by task id, so a rerun after a restart is not counted twice. */
const Calls = defineDoc<{ calls: string[] }>({
  kind: 'subagent.calls', version: 1, scope: 'conversation', history: 'latest', fork: 'initial', initial: () => ({ calls: [] }), checkpointWhen: () => false,
});

const failure = (text: string) => ({ output: [{ type: 'text' as const, text }], isError: true });

/** The `subagent` tool: a child conversation owned by the call (abort reaches it; a rerun finds it by owner), run to its answer. */
export function subagentExtension(): Extension {
  const extension: Extension = defineExtension({
    name: 'subagent',
    tools: [defineTool({
      name: 'subagent',
      description: 'Hand a self-contained task to a subagent and get its final answer back. It sees none of this conversation, so the task must carry everything it needs. Call it several times in one turn to run independent tasks in parallel.',
      parameters: obj({ task: str({ minLength: 1, description: 'The whole brief' }) }, { open: true }),
      replay: 'safe',
      async execute(args, api, ctx) {
        const child = await api.commit(async (tx) => {
          // The owner index remembers the child, so a rerun reuses it.
          const existing = (await tx.scanConversations({ ownerTaskId: api.taskId }, 1)).items[0];
          if (existing) return existing.id;
          const doc = await tx.doc(Calls, api.conversationId);
          if (doc.calls.length >= MAX_CHILDREN) return undefined;
          doc.calls = [...doc.calls, String(api.taskId)];
          const created = await tx.createConversation({ ownership: { kind: 'task', taskId: api.taskId } });
          await configure(tx, created.id, { extensions: { remove: [extension] } });
          return created.id;
        }, ctx);
        if (!child) return failure(`this session has already started ${MAX_CHILDREN} subagents`);
        await api.details({ conversationId: String(child) }, ctx);
        const conversation = (await api.conversation(child, ctx))!;
        const submission = await conversation.submit({ type: 'input', content: args.task, requestId: `subagent:${String(api.taskId)}` }, ctx);
        // The parent is not generating while its children are, so it gives its model slots back.
        const settled = await whileWaiting(() => submission.wait(ctx));
        if (settled.status !== 'done') return failure(`the subagent ended without an answer: ${settled.reason}`);
        const entry = await api.commit((tx) => tx.entry(AssistantEntry, settled.answer as never), ctx);
        const message = (entry as { model?: Array<{ content: Parameters<typeof contentText>[0] }> } | undefined)?.model?.[0];
        return { output: [{ type: 'text' as const, text: message ? contentText(message.content) : '' }] };
      },
    })],
  });
  return extension;
}

/** Selected by name in an agent's extensions map; the entry takes no config. */
export const manifest: ExtensionManifest = { name: 'subagent', session: () => [subagentExtension()] };
