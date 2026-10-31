import { join } from 'node:path';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { contentText, type MutableModels } from '@earendil-works/pi-ai';
import { AssistantEntry, createRegistry, Harness, type Extension } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';

const ctx = BACKGROUND_CONTEXT;

export type AgentSpec = {
  model: { provider: string; modelId: string };
  extensions?: Extension[];
};

export type SubmitResult =
  | { submissionId: string; status: 'queued' | 'placed' }
  | { submissionId: string; status: 'done'; answer: string }
  | { submissionId: string; status: 'unanswered'; reason: string; detail?: string };

// One process owns one session: its SQLite log is the only source of truth (A1, A6).
export async function openSession(opts: { dir: string; models: MutableModels; agent: AgentSpec }) {
  const registry = createRegistry();
  for (const ext of opts.agent.extensions ?? []) registry.install(ext);
  const storage = await openNodeSqliteStorage(join(opts.dir, 'session.sqlite'));
  const harness = await Harness.open(storage, { models: opts.models, registry }, ctx);
  const root = await harness.root(ctx, { agent: { model: opts.agent.model } });
  harness.resume();

  async function answerText(entryId: unknown): Promise<string> {
    const entry = await root.commit((tx) => tx.entry(AssistantEntry, entryId as never), ctx);
    const message = (entry as { model?: { content: Parameters<typeof contentText>[0] }[] } | undefined)?.model?.[0];
    return message ? contentText(message.content) : '';
  }

  return {
    harness,
    root,
    busy: () => harness.wake().active,
    async submit(input: string, requestId?: string, wait = false): Promise<SubmitResult> {
      const submission = await root.submit({ type: 'input', content: input, requestId }, ctx);
      const record = wait ? await submission.wait(ctx) : await submission.status(ctx);
      const submissionId = String(record.id);
      if (record.status === 'done') return { submissionId, status: 'done', answer: await answerText(record.answer) };
      if (record.status === 'unanswered') return { submissionId, status: 'unanswered', reason: String(record.reason), detail: record.detail === undefined ? undefined : String(record.detail) };
      return { submissionId, status: record.status };
    },
    close: () => harness.close(ctx),
  };
}

export type Session = Awaited<ReturnType<typeof openSession>>;
