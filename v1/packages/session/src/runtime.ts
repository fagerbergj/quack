import { join } from 'node:path';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { contentText, type MutableModels } from '@earendil-works/pi-ai';
import { AssistantEntry, createRegistry, defineExtension, Harness, section, watchEvents, type Conversation, type Extension } from '@earendil-works/pi-durable';
import { NodeExecutionEnv } from '@earendil-works/pi-durable/env/node';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import { mkdir } from 'node:fs/promises';
import { FINAL_TURNS, RunBudget } from '@quack/sdk';

const ctx = BACKGROUND_CONTEXT;

export type AgentSpec = {
  model: { provider: string; modelId: string };
  extensions?: Extension[];
  /** Model turns a run may take before it is aborted (HAR-10). */
  maxTurns?: number;
  instructions?: string;
};

export type SubmitResult =
  | { submissionId: string; status: 'queued' | 'placed' }
  | { submissionId: string; status: 'done'; answer: string }
  | { submissionId: string; status: 'unanswered'; reason: string; detail?: string };

/** The date line of every run's system prompt; a section is resent only when its text changes, so it costs one entry per day. */
export const clockExtension = (timeZone: string, now: () => Date) =>
  defineExtension({
    name: 'clock',
    sections: [section('date', () => `Today is ${formatDate(now(), timeZone)} (${timeZone}).`, { tag: false })],
  });

function formatDate(d: Date, timeZone: string): string {
  const part = (type: string) => new Intl.DateTimeFormat('en-US', { timeZone, [type]: type === 'year' ? 'numeric' : type === 'weekday' ? 'long' : '2-digit' }).formatToParts(d).find((p) => p.type === type)!.value;
  return `${part('weekday')}, ${part('year')}-${part('month')}-${part('day')}`;
}

/** An extension that takes over submissions in place of the agent loop, such as a bound workflow. */
export type Dispatch = (session: { harness: Harness; root: Conversation }, input: string, requestId: string | undefined, wait: boolean) => Promise<SubmitResult>;
export type DispatchingExtension = Extension & { dispatch?: Dispatch };

// One process owns one session: its SQLite log is the only source of truth (A1, A6).
export async function openSession(opts: { dir: string; models: MutableModels; agent: AgentSpec; timeZone?: string; now?: () => Date }) {
  const registry = createRegistry();
  registry.install(clockExtension(opts.timeZone ?? 'America/New_York', opts.now ?? (() => new Date())));
  for (const ext of opts.agent.extensions ?? []) registry.install(ext);
  const storage = await openNodeSqliteStorage(join(opts.dir, 'session.sqlite'));
  // The session's workspace lives beside its log on the same volume, so both move and expire together (RUN-2).
  const workspace = join(opts.dir, 'workspace');
  await mkdir(workspace, { recursive: true });
  const env = ({ cwd }: { cwd?: string }) => new NodeExecutionEnv({ cwd: cwd ?? workspace });
  const harness = await Harness.open(storage, { models: opts.models, registry, env }, ctx);
  const agent = { model: opts.agent.model, instructions: opts.agent.instructions };
  const root = await harness.root(ctx, { agent });
  // A session reopened under a newer definition takes its model and instructions; its history stays.
  const current = await root.agent(ctx);
  const stale = current.instructions !== agent.instructions || current.model?.provider !== agent.model.provider || current.model?.modelId !== agent.model.modelId;
  if (stale) await root.configure({ model: agent.model, instructions: agent.instructions ?? null }, ctx);
  harness.resume();
  const watchdog = opts.agent.maxTurns ? await turnLimit(harness, root, opts.agent.maxTurns) : undefined;

  const dispatch = (opts.agent.extensions as DispatchingExtension[] | undefined)?.find((e) => e.dispatch)?.dispatch;

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
      if (dispatch) return dispatch({ harness, root }, input, requestId, wait);
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

const FINAL_NOTICE = `Turn budget: ${FINAL_TURNS} turns left in this run. Stop investigating and deliver what you have now; a check that has not passed is released marked as not fully checked.`;

// Counts the current run's turns from the session's own events, publishes what is left, warns once before the end,
// and aborts the run past the limit; abort leaves it consistent.
async function turnLimit(harness: Harness, root: Awaited<ReturnType<Harness['root']>>, maxTurns: number) {
  const events = await watchEvents(harness, root.id, ctx);
  let turns = 0;
  const publish = (turnsLeft: number | null) => root.commit(async (tx) => { (await tx.doc(RunBudget, root.id)).turnsLeft = turnsLeft; }, ctx);
  events.start(async (batch) => {
    for (const e of batch) {
      // An overflowed watch re-syncs with a snapshot and drops turn events, so a snapshot restarts the count too.
      if (e.type === 'run_start' || e.type === 'snapshot') turns = 0;
      if (e.type === 'run_end') await publish(null);
      if (e.type !== 'turn_start') continue;
      if (++turns > maxTurns) void root.abort(ctx);
      else await publish(maxTurns - turns);
      if (maxTurns - turns === FINAL_TURNS && maxTurns > 2 * FINAL_TURNS) await root.submit({ type: 'input', content: FINAL_NOTICE, whenBusy: 'steer' }, ctx);
    }
  });
  return events;
}

export type Session = Awaited<ReturnType<typeof openSession>>;
