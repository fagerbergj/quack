import type { Context } from '@earendil-works/chord/context';
import { contentText, type Message } from '@earendil-works/pi-ai';
import { configure, type ToolExecutionApi } from '@earendil-works/pi-durable';

export type DecisionPoint = {
  /** Unique within the calling task; the decision is memoized under it, so a replay returns the same answer. */
  name: string;
  question: string;
  options: string[];
  evidence?: string;
  /** Below this confidence a resolver's answer escalates to the next resolver. */
  minConfidence?: number;
};

export type Decision = { choice: string; confidence: number; reason: string; resolver: string };

export type Resolver = {
  id: string;
  /** Returns undefined to escalate. */
  resolve(point: DecisionPoint, api: ToolExecutionApi, ctx: Context): Promise<Omit<Decision, 'resolver'> | undefined>;
};

export function prompt(point: DecisionPoint): string {
  const options = point.options.map((o) => `- ${o}`).join('\n');
  const evidence = point.evidence ? `\n\nEvidence:\n${point.evidence}` : '';
  return `Decision: ${point.question}\n\nOptions:\n${options}${evidence}\n\nAnswer with one JSON object and nothing else: {"choice": "<one option exactly>", "confidence": <0 to 1>, "reason": "<one sentence>"}`;
}

/** Parses a resolver's reply; undefined when it is not a valid answer to this point. */
export function parseDecision(text: string, point: DecisionPoint): Omit<Decision, 'resolver'> | undefined {
  const start = text.indexOf('{');
  const end = text.lastIndexOf('}');
  if (start < 0 || end <= start) return undefined;
  try {
    const { choice, confidence, reason } = JSON.parse(text.slice(start, end + 1));
    if (!point.options.includes(choice) || typeof confidence !== 'number' || confidence < 0 || confidence > 1) return undefined;
    return { choice, confidence, reason: typeof reason === 'string' ? reason : '' };
  } catch {
    return undefined;
  }
}

// Runs the chain until a resolver answers with enough confidence (GATE-1, GATE-2); undefined when every resolver escalated.
export async function decide(api: ToolExecutionApi, point: DecisionPoint, resolvers: Resolver[], ctx: Context): Promise<Decision | undefined> {
  const key = `decision:${point.name}`;
  const done = await api.memo<Decision>(key, ctx);
  if (done) return done;
  for (const r of resolvers) {
    const answer = await r.resolve(point, api, ctx);
    if (answer && answer.confidence >= (point.minConfidence ?? 0)) return api.memo<Decision>(key, { ...answer, resolver: r.id }, ctx);
  }
  return undefined;
}

type Tx = Parameters<Parameters<ToolExecutionApi['commit']>[0]>[0];

// The entry just before the newest assistant message: the in-flight tool round would leave unanswered tool calls in a fork.
async function completeTranscriptEnd(tx: Tx, conversationId: ToolExecutionApi['conversationId']) {
  const page = await tx.scanEntries({ conversationId, order: 'descending' }, 64);
  const lastAssistant = page.items.findIndex((e) => e.kind === 'pi.assistant');
  const at = page.items[lastAssistant + 1];
  if (lastAssistant < 0 || !at) throw new Error('no complete transcript to fork');
  return at.id;
}

// Default resolver (D18): the session's own model on a fork of the live transcript, tools off, so it sees everything and reuses the cache.
export const sessionModel: Resolver = {
  id: 'session',
  async resolve(point, api, ctx) {
    const forkKey = `decision-fork:${point.name}`;
    let forkId = await api.memo<number>(forkKey, ctx);
    if (!forkId) {
      const created = await api.commit(async (tx) => {
        const at = await completeTranscriptEnd(tx, api.conversationId);
        const fork = await tx.forkConversation(api.conversationId, at, { ownership: { kind: 'task', taskId: api.taskId } });
        await configure(tx, fork.id, { tools: [] });
        return Number(fork.id);
      }, ctx);
      forkId = await api.memo<number>(forkKey, created, ctx);
    }
    const fork = await api.conversation(forkId as never, ctx);
    if (!fork) return undefined;
    const submission = await fork.submit({ type: 'input', content: prompt(point), requestId: forkKey }, ctx);
    const settled = await submission.wait(ctx);
    if (settled.status !== 'done') return undefined;
    const answer = settled.answer === undefined ? undefined : await api.commit((tx) => tx.entry(settled.answer!), ctx);
    const message = (answer as { model?: Message[] } | undefined)?.model?.[0];
    return message && 'content' in message ? parseDecision(contentText(message.content as never), point) : undefined;
  },
};

/** A named model asked with only the question and evidence, such as a dedicated decision model (GATE-4). */
export function modelResolver(provider: string, modelId: string): Resolver {
  return {
    id: `${provider}/${modelId}`,
    async resolve(point, api, ctx) {
      const model = api.models.getModel(provider, modelId);
      if (!model) return undefined;
      const reply = await api.models.completeSimple(model, { messages: [{ role: 'user', content: prompt(point), timestamp: api.now() }] }, { signal: ctx.abortSignal });
      return parseDecision(contentText(reply.content), point);
    },
  };
}

/** The agent's own model asked with only the question and evidence: an independent view, as a checker should have. */
export const agentModel: Resolver = {
  id: 'agent-model',
  async resolve(point, api, ctx) {
    const ref = (await api.agent(ctx)).model;
    return ref ? modelResolver(ref.provider, ref.modelId).resolve(point, api, ctx) : undefined;
  },
};
