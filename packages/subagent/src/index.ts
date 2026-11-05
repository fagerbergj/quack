import { contentText } from '@earendil-works/pi-ai';
import { obj, opt, str } from '@earendil-works/pi-schema';
import { AssistantEntry, configure, defineDoc, defineExtension, defineTool, type Extension, type ModelRef } from '@earendil-works/pi-durable';
import { whileWaiting } from '@quack/sdk';
import { parseDefinition, type Catalog, type DeploymentConfig } from '@quack/session';
import type { SubagentsConfig } from './contract.ts';

export type { AgentSubagents, SubagentArgs, SubagentDetails, SubagentsConfig } from './contract.ts';

export const DEFAULT_LIMITS = { maxConcurrent: 4, maxTotal: 20 } satisfies Required<SubagentsConfig>;

/** What a child takes from another agent's definition. */
export type ChildAgent = { model: ModelRef; instructions?: string };
export type ResolveAgent = (name: string) => Promise<ChildAgent>;

/** The calls this session has admitted, by task id, so a rerun after a restart is not counted twice. */
const Calls = defineDoc<{ calls: string[] }>({
  kind: 'subagent.calls', version: 1, scope: 'conversation', history: 'latest', fork: 'initial', initial: () => ({ calls: [] }), checkpointWhen: () => false,
});

const failure = (text: string) => ({ output: [{ type: 'text' as const, text }], isError: true });

/** At most `size` holders at once; the rest queue in order and leave the queue when `signal` aborts. */
function semaphore(size: number) {
  let free = size;
  const queue: Array<() => void> = [];
  return async (signal?: AbortSignal): Promise<() => void> => {
    if (free > 0) free -= 1;
    else {
      signal?.throwIfAborted();
      await new Promise<void>((resolve, reject) => {
        const turn = () => (signal?.removeEventListener('abort', leave), resolve());
        const leave = () => (queue.splice(queue.indexOf(turn), 1), reject(signal!.reason));
        queue.push(turn);
        signal?.addEventListener('abort', leave, { once: true });
      });
    }
    return () => { const next = queue.shift(); if (next) next(); else free += 1; };
  };
}

/** Looks an agent up at the distributor and checks its definition against this deployment; the child takes its model and prompt. */
export function distributorAgents(opts: { distributorUrl: string; fetch?: typeof fetch; deployment: DeploymentConfig; catalog: Catalog }): ResolveAgent {
  return async (name) => {
    const res = await (opts.fetch ?? fetch)(`${opts.distributorUrl}/agents/${encodeURIComponent(name)}`);
    if (res.status === 404) throw new Error(`no agent named ${name}`);
    if (!res.ok) throw new Error(`looking up agent ${name} failed: HTTP ${res.status}`);
    const found = (await res.json()) as { definition: unknown; prompt?: string };
    const def = parseDefinition(found.definition, opts.deployment, opts.catalog);
    const slash = def.model.indexOf('/');
    return { model: { provider: def.model.slice(0, slash), modelId: def.model.slice(slash + 1) }, ...(found.prompt && { instructions: found.prompt }) };
  };
}

/** The `subagent` tool: a child conversation owned by the call (abort reaches it; a rerun finds it by owner), run to its answer. */
export function subagentExtension(limits: SubagentsConfig = {}, resolve?: ResolveAgent): Extension {
  const { maxConcurrent, maxTotal } = { ...DEFAULT_LIMITS, ...limits };
  const admit = semaphore(maxConcurrent);
  const extension: Extension = defineExtension({
    name: 'subagent',
    tools: [defineTool({
      name: 'subagent',
      description: 'Hand a self-contained task to a subagent and get its final answer back. It sees none of this conversation, so the task must carry everything it needs. Call it several times in one turn to run independent tasks in parallel. With `agent`, the subagent takes that agent\'s model and prompt.',
      parameters: obj({ task: str({ minLength: 1, description: 'The whole brief' }), agent: opt(str({ description: 'Another agent to take the model and prompt from' })) }, { open: true }),
      replay: 'safe',
      async execute(args, api, ctx) {
        let agent: ChildAgent | undefined;
        if (args.agent) {
          try {
            agent = await (resolve ?? (async () => { throw new Error('this session cannot look up other agents'); }))(args.agent);
          } catch (err) {
            return failure(err instanceof Error ? err.message : String(err));
          }
        }
        const release = await admit(ctx.abortSignal);
        try {
          const child = await api.commit(async (tx) => {
            // The owner index remembers the child, so a rerun reuses it.
            const existing = (await tx.scanConversations({ ownerTaskId: api.taskId }, 1)).items[0];
            if (existing) return existing.id;
            const doc = await tx.doc(Calls, api.conversationId);
            if (doc.calls.length >= maxTotal) return undefined;
            doc.calls = [...doc.calls, String(api.taskId)];
            const created = await tx.createConversation({ ownership: { kind: 'task', taskId: api.taskId } });
            await configure(tx, created.id, { extensions: { remove: [extension] }, ...(agent && { model: agent.model, instructions: agent.instructions }) });
            return created.id;
          }, ctx);
          if (!child) return failure(`this session has already started ${maxTotal} subagents`);
          await api.details({ conversationId: String(child) }, ctx);
          const conversation = (await api.conversation(child, ctx))!;
          const submission = await conversation.submit({ type: 'input', content: args.task, requestId: `subagent:${String(api.taskId)}` }, ctx);
          // The parent is not generating while its children are, so it gives its model slots back.
          const settled = await whileWaiting(() => submission.wait(ctx));
          if (settled.status !== 'done') return failure(`the subagent ended without an answer: ${settled.reason}`);
          const entry = await api.commit((tx) => tx.entry(AssistantEntry, settled.answer as never), ctx);
          const message = (entry as { model?: Array<{ content: Parameters<typeof contentText>[0] }> } | undefined)?.model?.[0];
          return { output: [{ type: 'text' as const, text: message ? contentText(message.content) : '' }] };
        } finally {
          release();
        }
      },
    })],
  });
  return extension;
}
