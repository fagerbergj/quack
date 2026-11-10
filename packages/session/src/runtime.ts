import { join } from 'node:path';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { contentText, type ImageContent, type MutableModels } from '@earendil-works/pi-ai';
import { AssistantEntry, createRegistry, defineExtension, GenerationTask, Harness, hook, InboxDoc, LiveDoc, section, type Conversation, type Extension, type Report, type SetTimer, type UserInput } from '@earendil-works/pi-durable';
import { openNodeSqliteStorage } from '@earendil-works/pi-durable/storage/sqlite/node';
import { randomUUID } from 'node:crypto';
import { mkdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import { Failures, MODEL_RETRY, pendingApprovals, Questions, statusNote, type Principal } from '@quack/sdk';
import { loopGuardExtension } from './loop-guard.ts';
import { cutOffExtension } from './cut-off.ts';
import { unknownToolExtension } from './unknown-tool.ts';
import { runCeilingExtension, TURN_CEILING } from './run-ceiling.ts';
import { CANNOT_SEE_IMAGES } from './images.ts';
import { LifecycleStream, recordTurn } from './lifecycle.ts';
import { resumeModelFault, watchModelFaults } from './model-fault.ts';
import { PolicyExecutionEnv, sessionPolicy, type ExecPolicy } from './exec.ts';
import { transcriptOf } from './export.ts';

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

export { MODEL_RETRY };

/** Who stopped or changed queued work, for the log line: the principal the distributor named, another service, or the run ceiling. */
export type Caller = Principal | 'service' | 'run-ceiling';

const REPORTED = new Set(['task_interrupted', 'task_faulted', 'extension_error', 'host_error']);

/** The log line for a harness report worth an operator's attention (an interrupted task otherwise ends without a trace); null for the rest. */
export function reportLine({ error, ...info }: Report): string | null {
  if (!REPORTED.has(info.kind)) return null;
  const at = info as { task?: { kind: string }; extension?: string; callback?: string; reason?: string };
  const where = [at.task && `task=${at.task.kind}`, at.extension && `extension=${at.extension}`, at.callback && `callback=${at.callback}`, at.reason && `reason=${at.reason}`].filter(Boolean);
  return `session: ${[info.kind, ...where].join(' ')}: ${error instanceof Error ? error.message : String(error)}`;
}

// One process owns one session: its SQLite log is the only source of truth.
export async function openSession(opts: { dir: string; models: MutableModels; agent: AgentSpec; exec?: ExecPolicy; timeZone?: string; now?: () => Date; setTimer?: SetTimer; retry?: Partial<typeof MODEL_RETRY>; log?: (line: string) => void; /** The session's key, for log lines. */ key?: string }) {
  const registry = createRegistry();
  registry.install(clockExtension(opts.timeZone ?? 'America/New_York', opts.now ?? (() => new Date())));
  registry.install(loopGuardExtension());
  registry.install(cutOffExtension());
  // The sink needs the harness, which opens after the registry; hooks only call it once a run is going.
  let open: Harness | undefined;
  let turnsOf: Conversation | undefined;
  const log = opts.log ?? console.error;
  const note = (event: string, fields: Record<string, unknown>) => log(JSON.stringify({ event: `session.${event}`, key: opts.key, ...fields }));
  registry.install(unknownToolExtension(async (id) => (await (await open!.conversation(id, ctx))!.agent(ctx)).tools.map((t) => t.name)));
  registry.install(runCeilingExtension(opts.agent.turnCeiling ?? TURN_CEILING, {
    async record(id, run, error) {
      const conversation = await open!.conversation(id, ctx);
      await conversation!.commit(async (tx) => { (await tx.doc(Failures, id)).items.runaway = { error, run: Number(run) || 0, at: Date.now() }; }, ctx);
      // Not awaited: the abort waits for the hook's own task to settle.
      if (error) void stopRun(conversation!).then((r) => note('abort', { caller: 'run-ceiling', ...r }), () => undefined);
    },
  }));
  // A tool round's end goes in the root's lifecycle stream, the catalog's outbox, as turn.ended (lifecycle channel v10).
  registry.install(defineExtension({ name: 'turn-ends', hooks: [hook(GenerationTask, {
    async afterTools(assistant, _results, api) {
      const root = turnsOf;
      if (api.conversationId !== root?.id) return;
      await root.commit(async (tx) => recordTurn(await tx.doc(LifecycleStream, root.id), String(assistant), +(opts.now?.() ?? Date.now())), ctx);
    },
  })] }));
  for (const ext of opts.agent.extensions ?? []) registry.install(ext);
  const storage = await openNodeSqliteStorage(join(opts.dir, 'session.sqlite'));
  // The session's workspace lives beside its log on the same volume, so both move and expire together (RUN-2).
  const workspace = join(opts.dir, 'workspace');
  await mkdir(workspace, { recursive: true });
  const sees = !!opts.models.getModel(opts.agent.model.provider, opts.agent.model.modelId)?.input.includes('image');
  const env = ({ cwd }: { cwd?: string }) => new PolicyExecutionEnv({ cwd: cwd ?? workspace, policy: sessionPolicy(opts.exec, opts.agent.commandTimeoutSeconds) });
  const onReport = (report: Report) => {
    const line = reportLine(report);
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
    // A failed run starts the inputs queued behind it instead of leaving them queued with nothing to run them;
    // queued steers and follow-ups reach the model together in one turn, not one per turn.
    settings: { retry: { ...MODEL_RETRY, ...opts.retry }, afterFailedRun: 'continue', steeringMode: 'all', followUpMode: 'all' },
  }, ctx);
  open = harness;
  const agent = { model: opts.agent.model, instructions: opts.agent.instructions };
  const root = await harness.root(ctx, { agent });
  turnsOf = root;
  // A session reopened under a newer definition takes its model and instructions; its history stays.
  const current = await root.agent(ctx);
  const stale = current.instructions !== agent.instructions || current.model?.provider !== agent.model.provider || current.model?.modelId !== agent.model.modelId;
  if (stale) await root.configure({ model: agent.model, instructions: agent.instructions ?? null }, ctx);
  harness.resume();
  await watchModelFaults(harness, root, log);
  // A process that stopped mid-abort left the queue with nothing to run it. A close ends the wait; a storage error reaches onReport.
  void root.waitForIdle(ctx).then(() => startQueued(root, false)).catch(() => undefined);

  // Stops the conversation's current work only. Conversation.abort() withdraws queued inputs, so this aborts the work's
  // tasks instead and lets the inputs queued behind it start the next run.
  async function stopRun(c: Conversation) {
    const inputs = (await harness.snapshot(LiveDoc, c.id, ctx))?.run?.inputs ?? [];
    const top = (await harness.inspect(ctx)).tasks.map((t) => t.record).filter((t) => t.conversationId === c.id && t.owner === undefined && !t.background);
    await Promise.all(top.map((t) => harness.abortTask(t.id, ctx)));
    await Promise.all(top.map((t) => harness.waitForTask(t.id, ctx)));
    // A run that answered before its abort mark landed settled its inputs done; only those the abort settled count.
    const settled = await Promise.all(inputs.map(async (id) => (await (await harness.submission(id, ctx))?.status(ctx))));
    const aborted = settled.filter((r) => r?.status === 'unanswered' && r.reason === 'aborted').map((r) => String(r!.id));
    return { aborted, ...(await startQueued(c, true)) };
  }

  // A continue places the queued inputs, oldest first, as the next run's input; in one commit, so a racing input or withdraw wins cleanly.
  // A run that went live during the abort (the stopped run answered first and its end placed the queue) is the next run already.
  function startQueued(c: Conversation, closing: boolean) {
    return c.commit(async (tx) => {
      const kept = (await tx.doc(InboxDoc, c.id)).items.filter((i) => i.mode !== 'write').map((i) => String(i.id));
      const run = (await tx.doc(LiveDoc, c.id)).run;
      if (run) return { kept, queue: 'placed by the run already going', running: run.inputs.map(String) };
      // The stopped run's questions close before the next run starts, so a question that run asks stays open.
      if (closing) for (const q of Object.values((await tx.doc(Questions, c.id)).items)) if (!q.answer) q.closed = true;
      if (kept.length) await c.submit(tx, { type: 'continue' });
      return { kept, queue: kept.length ? 'started the next run' : 'empty' };
    }, ctx);
  }

  const dispatch = (opts.agent.extensions as DispatchingExtension[] | undefined)?.find((e) => e.dispatch)?.dispatch;
  const resume = (opts.agent.extensions as DispatchingExtension[] | undefined)?.find((e) => e.resume)?.resume;

  async function answerText(entryId: unknown): Promise<string> {
    const entry = await root.commit((tx) => tx.entry(AssistantEntry, entryId as never), ctx);
    const message = (entry as { model?: { content: Parameters<typeof contentText>[0] }[] } | undefined)?.model?.[0];
    return message ? contentText(message.content) : '';
  }

  // True when every task that can run is a gated tool waiting for a verdict; a verdict wakes the process through the normal request path.
  async function onlyAwaitingHumans(): Promise<boolean> {
    const live = (await harness.inspect(ctx)).tasks.filter((t) => t.state.kind === 'running' || t.state.kind === 'pending');
    const pending = new Set((await pendingApprovals(root, ctx)).map((a) => a.id));
    return live.length > 0 && live.every((t) => pending.has(String(t.record.id)));
  }

  let metaWrites = Promise.resolve();
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
    // Best effort, a failed write dropped; writes run in turn so records arriving together all land, and a renamed unique temp file survives a crash.
    recordMeta(record: Record<string, unknown>) {
      const write = metaWrites.then(async () => {
        try {
          const kept = (await this.meta()).filter((r) => r.id !== record.id);
          const file = join(opts.dir, 'meta.json');
          const temp = `${file}.${randomUUID()}.tmp`;
          await writeFile(temp, JSON.stringify([...kept, record]));
          await rename(temp, file).catch(async (err) => { await rm(temp, { force: true }); throw err; });
        } catch {
          // the review runs without fresh meta
        }
      });
      metaWrites = write;
      return write;
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
    // `conversation` reads another of this session's conversations; callers check it with `readable` first.
    async messages(limit: number, conversation?: string): Promise<Array<{ role: 'user' | 'assistant'; text: string }>> {
      const found: Array<{ role: 'user' | 'assistant'; text: string }> = [];
      let cursor: unknown;
      do {
        const page = await root.commit((tx) => tx.scanEntries({ conversationId: (conversation ?? root.id) as never, order: 'descending' }, 64, cursor as never), ctx);
        for (const e of page.items) {
          const m = e.model?.[0] as { content?: Parameters<typeof contentText>[0] } | undefined;
          if ((e.kind === 'pi.user' || e.kind === 'pi.assistant') && m?.content && found.length < limit) found.push({ role: e.kind === 'pi.user' ? 'user' : 'assistant', text: contentText(m.content) });
        }
        cursor = page.next as never;
      } while (cursor && found.length < limit);
      return found.reverse();
    },
    // The root or a created child such as a subagent's; a fork (a decision's) is task-owned too, so creation is the test, not ownership.
    async readable(conversation: string): Promise<boolean> {
      if (conversation === String(root.id)) return true;
      const found = await root.commit((tx) => tx.conversation(conversation as never), ctx);
      return Boolean(found?.owner && !found.parent);
    },
    transcript: () => transcriptOf(root),
    // The follow-ups waiting for the run to answer, with their text: the inbox item itself is the durable queue.
    async queued() {
      return (await this.queuedItems()).map(({ id, text }) => ({ id, text }));
    },
    async queuedItems() {
      const inbox = await harness.snapshot(InboxDoc, root.id, ctx);
      return (inbox?.items ?? []).flatMap((i) => (i.mode === 'followUp' ? [{ id: String(i.id), text: contentText(i.content as Parameters<typeof contentText>[0]), content: i.content as UserInput }] : []));
    },
    // Stops the current run; input queued behind it is kept and starts the next run.
    async abort(caller: Caller) {
      const result = await stopRun(root);
      note('abort', { caller, ...result });
      return result;
    },
    // Withdraws a queued follow-up; false when the run already took it. With `steer`, sends its text again as a steer.
    async withdraw(id: string, steer = false, caller: Caller = 'service'): Promise<boolean> {
      const item = (await this.queuedItems()).find((q) => q.id === id);
      const submission = item && (await harness.submission(id as never, ctx));
      const removed = !!item && (await submission!.abort(ctx)) === 'aborted';
      const steered = removed && steer ? (await this.submitContent(item.content, undefined, false, 'steer')).submissionId : undefined;
      note(steer ? 'steer' : 'withdraw', { caller, submission: id, queue: removed ? 'removed' : 'not queued', ...(steered && { steered }) });
      return removed;
    },
    // Images ride in the user message, so the transcript shows them after a reload; a model without image input is refused.
    async submit(input: string, requestId?: string, wait = false, whenBusy?: 'steer' | 'followUp', images: ImageContent[] = []): Promise<SubmitResult> {
      if (!images.length) return this.submitContent(input, requestId, wait, whenBusy);
      if (!sees) throw new Error(CANNOT_SEE_IMAGES);
      return this.submitContent([...(input ? [{ type: 'text' as const, text: input }] : []), ...images], requestId, wait, whenBusy);
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
    resume: async () => (await resume?.({ harness, root })) ?? (await resumeModelFault(root, harness)),
    close: async () => {
      await harness.close(ctx);
    },
  };
}

export type Session = Awaited<ReturnType<typeof openSession>>;
