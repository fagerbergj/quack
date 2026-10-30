import { join } from 'node:path';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { contentText, type MutableModels } from '@earendil-works/pi-ai';
import { AssistantEntry, createRegistry, Harness, watchEvents, type Extension } from '@earendil-works/pi-durable';
import { NodeExecutionEnv } from '@earendil-works/pi-durable/env/node';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import { mkdir } from 'node:fs/promises';

const ctx = BACKGROUND_CONTEXT;

export type AgentSpec = {
  model: { provider: string; modelId: string };
  extensions?: Extension[];
  /** Model turns a run may take before it is aborted (HAR-10). */
  maxTurns?: number;
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
  // The session's workspace lives beside its log on the same volume, so both move and expire together (RUN-2).
  const workspace = join(opts.dir, 'workspace');
  await mkdir(workspace, { recursive: true });
  const env = ({ cwd }: { cwd?: string }) => new NodeExecutionEnv({ cwd: cwd ?? workspace });
  const harness = await Harness.open(storage, { models: opts.models, registry, env }, ctx);
  const root = await harness.root(ctx, { agent: { model: opts.agent.model } });
  harness.resume();
  const watchdog = opts.agent.maxTurns ? await turnLimit(harness, root, opts.agent.maxTurns) : undefined;

  async function answerText(entryId: unknown): Promise<string> {
    const entry = await root.commit((tx) => tx.entry(AssistantEntry, entryId as never), ctx);
    const message = (entry as { model?: { content: Parameters<typeof contentText>[0] }[] } | undefined)?.model?.[0];
    return message ? contentText(message.content) : '';
  }

  return {
    harness,
    workspace,
    root,
    busy: () => harness.wake().active,
    async submit(input: string, requestId?: string, wait = false, whenBusy?: 'steer' | 'followUp'): Promise<SubmitResult> {
      const submission = await root.submit({ type: 'input', content: input, requestId, whenBusy }, ctx);
      const record = wait ? await submission.wait(ctx) : await submission.status(ctx);
      const submissionId = String(record.id);
      if (record.status === 'done') return { submissionId, status: 'done', answer: await answerText(record.answer) };
      if (record.status === 'unanswered') return { submissionId, status: 'unanswered', reason: String(record.reason), detail: record.detail === undefined ? undefined : String(record.detail) };
      return { submissionId, status: record.status };
    },
    close: async () => {
      await watchdog?.stop();
      await harness.close(ctx);
    },
  };
}

// Counts the current run's turns from the session's own events and aborts the run past the limit; abort leaves it consistent.
async function turnLimit(harness: Harness, root: Awaited<ReturnType<Harness['root']>>, maxTurns: number) {
  const events = await watchEvents(harness, root.id, ctx);
  let turns = 0;
  events.start(async (batch) => {
    for (const e of batch) {
      if (e.type === 'run_start') turns = 0;
      if (e.type === 'turn_start' && ++turns > maxTurns) void root.abort(ctx);
    }
  });
  return events;
}

export type Session = Awaited<ReturnType<typeof openSession>>;
