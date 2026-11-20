import type { Context } from '@earendil-works/chord/context';
import type { JsonValue } from '@earendil-works/chord';
import type { ToolExecutionApi, ToolRegistration } from '@earendil-works/pi-durable';
import { WorkflowDoc } from './doc.ts';

/** What a workflow offers a person's message in its session: a system prompt for the chat turn and read tools over the session. */
export type Chat = { prompt: string; tools: ToolRegistration[] };

/** The latest run's step results (a finished run's, or what a failed one kept) and the input it started from. */
export async function latestRun(api: Pick<ToolExecutionApi, 'conversationId' | 'snapshot' | 'getTask'>, ctx: Context) {
  const doc = await api.snapshot(WorkflowDoc, api.conversationId, ctx);
  const task = doc?.task ? await api.getTask(Number(doc.task) as never, ctx) : undefined;
  if (!doc || !task) return undefined;
  const { outcome } = task.state as { outcome?: { status: string; result?: Record<string, JsonValue> } };
  const results = outcome?.status === 'completed' ? outcome.result : doc.kept;
  return { status: outcome?.status ?? task.state.status, steps: doc.steps, results: results ?? {}, input: (task.input as { input: JsonValue }).input };
}
