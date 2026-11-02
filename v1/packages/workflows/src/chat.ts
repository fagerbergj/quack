import type { Context } from '@earendil-works/chord/context';
import type { JsonValue } from '@earendil-works/chord';
import type { Conversation, Harness, ToolRegistration } from '@earendil-works/pi-durable';
import { WorkflowDoc } from './doc.ts';

export type ChatSession = { harness: Harness; root: Conversation };

/** What a workflow offers a person's message in its session: a system prompt for the chat turn and read tools over the session. */
export type Chat = { prompt: string; tools: (session: () => ChatSession) => ToolRegistration[] };

/** The latest run's step results (a finished run's, or what a failed one kept) and the input it started from. */
export async function latestRun(s: ChatSession, ctx: Context) {
  const doc = await s.harness.snapshot(WorkflowDoc, s.root.id, ctx);
  const task = doc?.task ? await s.harness.getTask(Number(doc.task) as never, ctx) : undefined;
  if (!doc || !task) return undefined;
  const { outcome } = task.state as { outcome?: { status: string; result?: Record<string, JsonValue> } };
  const results = outcome?.status === 'completed' ? outcome.result : doc.kept;
  return { status: outcome?.status ?? task.state.status, steps: doc.steps, results: results ?? {}, input: (task.input as { input: JsonValue }).input };
}
