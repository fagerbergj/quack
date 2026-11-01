import { join } from 'node:path';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { contentText, type MutableModels } from '@earendil-works/pi-ai';
import { AssistantEntry, createRegistry, defineExtension, Harness, section, type Conversation, type Extension, type ReportInfo, type SetTimer } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import { mkdir, writeFile } from 'node:fs/promises';
import { pendingApprovals } from '@quack/sdk';
import { PolicyExecutionEnv, sessionPolicy, type ExecPolicy } from './exec.ts';

const ctx = BACKGROUND_CONTEXT;

export type AgentSpec = {
  model: { provider: string; modelId: string };
  extensions?: Extension[];
  instructions?: string;
  /** From the agent's bundle: shortens the deployment's command timeout, never lengthens it. */
  commandTimeoutSeconds?: number;
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

// pi-durable's default (3 retries from 2s) gives up inside 15s; this rides out a model host that is down for about 5 minutes.
export const MODEL_RETRY = { enabled: true, maxRetries: 9, baseDelayMs: 2000, maxAgentDelayMs: 60_000 };

const REPORTED = new Set(['task_parked', 'task_faulted', 'extension_error', 'host_error']);

/** The log line for a harness report worth an operator's attention (a parked task is otherwise silently blocked); null for the rest. */
export function reportLine(error: unknown, info: ReportInfo): string | null {
  if (!REPORTED.has(info.type)) return null;
  const at = info as { task?: { kind: string }; extension?: string; callback?: string; reason?: string };
  const where = [at.task && `task=${at.task.kind}`, at.extension && `extension=${at.extension}`, at.callback && `callback=${at.callback}`, at.reason && `reason=${at.reason}`].filter(Boolean);
  return `session: ${[info.type, ...where].join(' ')}: ${error instanceof Error ? error.message : String(error)}`;
}

// One process owns one session: its SQLite log is the only source of truth (A1, A6).
export async function openSession(opts: { dir: string; models: MutableModels; agent: AgentSpec; exec?: ExecPolicy; timeZone?: string; now?: () => Date; setTimer?: SetTimer; retry?: Partial<typeof MODEL_RETRY>; log?: (line: string) => void }) {
  const registry = createRegistry();
  registry.install(clockExtension(opts.timeZone ?? 'America/New_York', opts.now ?? (() => new Date())));
  for (const ext of opts.agent.extensions ?? []) registry.install(ext);
  const storage = await openNodeSqliteStorage(join(opts.dir, 'session.sqlite'));
  // The session's workspace lives beside its log on the same volume, so both move and expire together (RUN-2).
  const workspace = join(opts.dir, 'workspace');
  await mkdir(workspace, { recursive: true });
  const env = ({ cwd }: { cwd?: string }) => new PolicyExecutionEnv({ cwd: cwd ?? workspace, policy: sessionPolicy(opts.exec, opts.agent.commandTimeoutSeconds) });
  const log = opts.log ?? console.error;
  const onReport = (error: unknown, info: ReportInfo) => {
    const line = reportLine(error, info);
    if (line) log(line);
  };
  const harness = await Harness.open(storage, {
    models: opts.models,
    registry,
    env,
    onReport,
    // A test drives sleeps on a fake clock: the Harness reads the same `now` and arms its timers through `setTimer`.
    ...(opts.now && { now: () => +opts.now!() }),
    ...(opts.setTimer && { setTimer: opts.setTimer }),
    // A failed run starts the inputs queued behind it instead of leaving them queued with nothing to run them.
    settings: { retry: { ...MODEL_RETRY, ...opts.retry }, afterFailedRun: 'continue' },
  }, ctx);
  const agent = { model: opts.agent.model, instructions: opts.agent.instructions };
  const root = await harness.root(ctx, { agent });
  // A session reopened under a newer definition takes its model and instructions; its history stays.
  const current = await root.agent(ctx);
  const stale = current.instructions !== agent.instructions || current.model?.provider !== agent.model.provider || current.model?.modelId !== agent.model.modelId;
  if (stale) await root.configure({ model: agent.model, instructions: agent.instructions ?? null }, ctx);
  harness.resume();

  const dispatch = (opts.agent.extensions as DispatchingExtension[] | undefined)?.find((e) => e.dispatch)?.dispatch;

  async function answerText(entryId: unknown): Promise<string> {
    const entry = await root.commit((tx) => tx.entry(AssistantEntry, entryId as never), ctx);
    const message = (entry as { model?: { content: Parameters<typeof contentText>[0] }[] } | undefined)?.model?.[0];
    return message ? contentText(message.content) : '';
  }

  // True when every task that can run is a gated tool waiting for a verdict; a verdict wakes the process through the normal request path.
  async function onlyAwaitingHumans(): Promise<boolean> {
    const live = (await harness.inspect(ctx)).tasks.filter((t) => t.state.kind === 'running' || t.state.kind === 'ready');
    const pending = new Set((await pendingApprovals(root, ctx)).map((a) => a.id));
    return live.length > 0 && live.every((t) => pending.has(String(t.record.id)));
  }

  return {
    harness,
    workspace,
    root,
    // For the idle sweep: `busy` is work that must keep the process, `wakeAt` the earliest sleep to be woken for.
    async standing() {
      const { active, at: wakeAt } = harness.wake();
      const waitingOnHuman = active && (await onlyAwaitingHumans());
      return { busy: active && !waitingOnHuman, waitingOnHuman, wakeAt };
    },
    // The caller's name for the session (a PR title); the first one sticks, and the distributor lists it ahead of the first message.
    async recordTitle(title: string) {
      await writeFile(join(opts.dir, 'title'), title.replace(/\s+/g, ' ').trim().slice(0, 200), { flag: 'wx' }).catch(() => undefined);
    },
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
      await harness.close(ctx);
    },
  };
}

export type Session = Awaited<ReturnType<typeof openSession>>;
