import { join } from 'node:path';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { contentText, type ImageContent, type MutableModels } from '@earendil-works/pi-ai';
import { AssistantEntry, createRegistry, defineExtension, Harness, InboxDoc, section, type Conversation, type Extension, type ReportInfo, type SetTimer, type UserInput } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { Failures, pendingApprovals, statusNote } from '@quack/sdk';
import { loopGuardExtension } from './loop-guard.ts';
import { runCeilingExtension, TURN_CEILING } from './run-ceiling.ts';
import { lookExtension, NO_IMAGE_MODEL, saveImages } from './images.ts';
import { PolicyExecutionEnv, sessionPolicy, type ExecPolicy } from './exec.ts';

const ctx = BACKGROUND_CONTEXT;

export type AgentSpec = {
  model: { provider: string; modelId: string };
  extensions?: Extension[];
  instructions?: string;
  /** From the agent's bundle: shortens the deployment's command timeout, never lengthens it. */
  commandTimeoutSeconds?: number;
  /** Model responses one run may take before it is failed (default TURN_CEILING). */
  turnCeiling?: number;
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

/** An extension that takes over submissions in place of the agent loop, such as a bound workflow; undefined leaves one to the loop. */
export type Dispatch = (session: { harness: Harness; root: Conversation }, input: string, requestId: string | undefined, wait: boolean) => Promise<SubmitResult | undefined>;
/** Reruns the session's failed run from where it stopped; null when nothing failed. */
export type Resume = (session: { harness: Harness; root: Conversation }) => Promise<SubmitResult | null>;
export type DispatchingExtension = Extension & { dispatch?: Dispatch; resume?: Resume };

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
export async function openSession(opts: { dir: string; models: MutableModels; agent: AgentSpec; exec?: ExecPolicy; /** Answers questions about images for an agent whose own model cannot see them. */ imageModel?: { provider: string; modelId: string }; timeZone?: string; now?: () => Date; setTimer?: SetTimer; retry?: Partial<typeof MODEL_RETRY>; log?: (line: string) => void }) {
  const registry = createRegistry();
  registry.install(clockExtension(opts.timeZone ?? 'America/New_York', opts.now ?? (() => new Date())));
  registry.install(loopGuardExtension());
  // The sink needs the harness, which opens after the registry; hooks only call it once a run is going.
  let open: Harness | undefined;
  registry.install(runCeilingExtension(opts.agent.turnCeiling ?? TURN_CEILING, {
    async record(id, run, error) {
      const conversation = await open!.conversation(id, ctx);
      await conversation!.commit(async (tx) => { (await tx.doc(Failures, id)).items.runaway = { error, run: Number(run) || 0, at: Date.now() }; }, ctx);
      // Not awaited: abort waits for the hook's own task to settle.
      if (error) void conversation!.abort(ctx).catch(() => undefined);
    },
  }));
  for (const ext of opts.agent.extensions ?? []) registry.install(ext);
  const storage = await openNodeSqliteStorage(join(opts.dir, 'session.sqlite'));
  // The session's workspace lives beside its log on the same volume, so both move and expire together (RUN-2).
  const workspace = join(opts.dir, 'workspace');
  await mkdir(workspace, { recursive: true });
  const sees = !!opts.models.getModel(opts.agent.model.provider, opts.agent.model.modelId)?.input.includes('image');
  if (!sees && opts.imageModel) registry.install(lookExtension(workspace, opts.models, opts.imageModel));
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
  open = harness;
  const agent = { model: opts.agent.model, instructions: opts.agent.instructions };
  const root = await harness.root(ctx, { agent });
  // A session reopened under a newer definition takes its model and instructions; its history stays.
  const current = await root.agent(ctx);
  const stale = current.instructions !== agent.instructions || current.model?.provider !== agent.model.provider || current.model?.modelId !== agent.model.modelId;
  if (stale) await root.configure({ model: agent.model, instructions: agent.instructions ?? null }, ctx);
  harness.resume();

  const dispatch = (opts.agent.extensions as DispatchingExtension[] | undefined)?.find((e) => e.dispatch)?.dispatch;
  const resume = (opts.agent.extensions as DispatchingExtension[] | undefined)?.find((e) => e.resume)?.resume;

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
      return { busy: active && !waitingOnHuman, waitingOnHuman, wakeAt, note: statusNote.get() };
    },
    // The caller's name for the session (a PR title); the first one sticks, and the distributor lists it ahead of the first message.
    async recordTitle(title: string) {
      await writeFile(join(opts.dir, 'title'), title.replace(/\s+/g, ' ').trim().slice(0, 200), { flag: 'wx' }).catch(() => undefined);
    },
    // Data the UI reads beside the transcript (a PR's files and checks); one record per `id`, a later one replacing the earlier.
    // Best effort: a run never waits on it, so a failed read or write is dropped, and the rename keeps a crash from leaving half a file.
    async recordMeta(record: Record<string, unknown>) {
      try {
        const kept = (await this.meta()).filter((r) => r.id !== record.id);
        const file = join(opts.dir, 'meta.json');
        await writeFile(`${file}.tmp`, JSON.stringify([...kept, record]));
        await rename(`${file}.tmp`, file);
      } catch {
        // the review runs without fresh meta
      }
    },
    async meta(): Promise<Array<Record<string, unknown>>> {
      try {
        const found: unknown = JSON.parse(await readFile(join(opts.dir, 'meta.json'), 'utf8'));
        return Array.isArray(found) ? found : [];
      } catch {
        return [];
      }
    },
    // The newest `limit` person and assistant messages, oldest first, so a reader need not hold the socket open for the transcript.
    async messages(limit: number): Promise<Array<{ role: 'user' | 'assistant'; text: string }>> {
      const found: Array<{ role: 'user' | 'assistant'; text: string }> = [];
      let cursor: unknown;
      do {
        const page = await root.commit((tx) => tx.scanEntries({ conversationId: root.id, order: 'descending' }, 64, cursor as never), ctx);
        for (const e of page.items) {
          const m = e.model?.[0] as { content?: Parameters<typeof contentText>[0] } | undefined;
          if ((e.kind === 'pi.user' || e.kind === 'pi.assistant') && m?.content && found.length < limit) found.push({ role: e.kind === 'pi.user' ? 'user' : 'assistant', text: contentText(m.content) });
        }
        cursor = page.next as never;
      } while (cursor && found.length < limit);
      return found.reverse();
    },
    // The follow-ups waiting for the run to answer, with their text: the inbox item itself is the durable queue.
    async queued() {
      return (await this.queuedItems()).map(({ id, text }) => ({ id, text }));
    },
    async queuedItems() {
      const inbox = await harness.snapshot(InboxDoc, root.id, ctx);
      return (inbox?.items ?? []).flatMap((i) => (i.mode === 'followUp' ? [{ id: String(i.id), text: contentText(i.content as Parameters<typeof contentText>[0]), content: i.content as UserInput }] : []));
    },
    // Withdraws a queued follow-up; false when the run already took it. With `steer`, sends its text again as a steer.
    async withdraw(id: string, steer = false): Promise<boolean> {
      const item = (await this.queuedItems()).find((q) => q.id === id);
      const submission = item && (await harness.submission(id as never, ctx));
      if (!item || (await submission!.abort(ctx)) !== 'aborted') return false;
      if (steer) await this.submitContent(item.content, undefined, false, 'steer');
      return true;
    },
    // Images ride in the user message, so the transcript shows them after a reload; a model without image input gets a placeholder from pi-ai and asks look_at_image about the saved copy.
    async submit(input: string, requestId?: string, wait = false, whenBusy?: 'steer' | 'followUp', images: ImageContent[] = []): Promise<SubmitResult> {
      if (!images.length) return this.submitContent(input, requestId, wait, whenBusy);
      if (!sees && !opts.imageModel) throw new Error(NO_IMAGE_MODEL);
      const note = sees ? '' : `\n\n[Attached images ${(await saveImages(workspace, images)).join(', ')}. You cannot see images: use look_at_image to ask about one.]`;
      const text = `${input}${note}`.trim();
      return this.submitContent([...(text ? [{ type: 'text' as const, text }] : []), ...images], requestId, wait, whenBusy);
    },
    async submitContent(content: UserInput, requestId?: string, wait = false, whenBusy?: 'steer' | 'followUp'): Promise<SubmitResult> {
      const taken = typeof content === 'string' ? await dispatch?.({ harness, root }, content, requestId, wait) : undefined;
      if (taken) return taken;
      const submission = await root.submit({ type: 'input', content, requestId, whenBusy }, ctx);
      const record = wait ? await submission.wait(ctx) : await submission.status(ctx);
      const submissionId = String(record.id);
      if (record.status === 'done') return { submissionId, status: 'done', answer: await answerText(record.answer) };
      if (record.status === 'unanswered') return { submissionId, status: 'unanswered', reason: String(record.reason), detail: record.detail === undefined ? undefined : String(record.detail) };
      return { submissionId, status: record.status };
    },
    resume: async () => (await resume?.({ harness, root })) ?? null,
    close: async () => {
      await harness.close(ctx);
    },
  };
}

export type Session = Awaited<ReturnType<typeof openSession>>;
