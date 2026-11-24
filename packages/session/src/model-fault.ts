import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { retryDelayMs } from '@earendil-works/pi-ai';
import { ConversationBusy, LiveDoc, type ConversationRetryPolicy, type Cursor, type LiveState, type Conversation, type Harness } from '@earendil-works/pi-durable';
import { Failures, modelWait, transientModelError } from '@quack/sdk';

const ctx = BACKGROUND_CONTEXT;
export const MODEL_STEP = 'model';

/** How long a run rides out a model endpoint that is down, refusing or overloaded before it ends in error. */
export const MODEL_OUTAGE_HOURS = 6;
// Tries back off from 2 s to one every 5 minutes; each is the real request, since a router's own /models answers while its upstream is dead.
const BACKOFF = { enabled: true, baseDelayMs: 2000, maxAgentDelayMs: 300_000 };

/** The harness retry policy: as many tries as the backoff fits within `hours`. A 4xx or an input over the context is never retried. */
export function outageRetry(hours = MODEL_OUTAGE_HOURS): ConversationRetryPolicy {
  let maxRetries = 0;
  for (let waited = 0; waited + retryDelayMs(BACKOFF, maxRetries + 1) <= hours * 3_600_000; ) waited += retryDelayMs(BACKOFF, ++maxRetries);
  return { ...BACKOFF, maxRetries };
}

/** A runtime's window: QUACK_MODEL_OUTAGE_HOURS (an eval's short one) over the deployment's modelOutageHours over MODEL_OUTAGE_HOURS. */
export function outageHoursOf(deployment: { modelOutageHours?: number }, env: Record<string, string | undefined> = process.env): number {
  const raw = env.QUACK_MODEL_OUTAGE_HOURS;
  if (raw === undefined) return deployment.modelOutageHours ?? MODEL_OUTAGE_HOURS;
  const hours = Number(raw);
  if (!(hours > 0 && hours <= 168)) throw new Error(`QUACK_MODEL_OUTAGE_HOURS must be a number of hours above 0 and at most 168, not ${raw}`);
  return hours;
}

function windowText(policy: ConversationRetryPolicy): string {
  let ms = 0;
  for (let n = 1; n <= policy.maxRetries; n++) ms += retryDelayMs(policy, n);
  return ms >= 3_600_000 ? `${Math.round(ms / 3_600_000)} h` : `${Math.max(1, Math.round(ms / 60_000))} min`;
}

// pi-durable retries only the errors pi-ai names transient; a provider abort is not one of them.
const isAbort = (detail?: string) => /finish_reason:\s*abort/i.test(detail ?? '');

async function setRow(root: Conversation, error: string | null, run = 0) {
  await root.commit(async (tx) => {
    const doc = await tx.doc(Failures, root.id);
    const row = doc.items[MODEL_STEP];
    if (error === null ? row?.error : row?.error !== error || row.run !== run) doc.items[MODEL_STEP] = { error, run, at: Date.now() };
  }, ctx);
}

/** What the session's model endpoint and retry policy are, for the waiting signal and the failure text. `provider` is the resolved model's
 * (resolveModel: a session's own pick first), which its subagent children run too, so every parked conversation is labelled with it. */
export type ModelFaultSetup = { provider: string; retry: ConversationRetryPolicy; now: () => number };

// A transient error that still ended the run used up the whole retry window, so the row says how long it retried.
function failureText(detail: string | undefined, setup: ModelFaultSetup): string {
  if (!detail) return 'The model failed.';
  if (!transientModelError({ stopReason: 'error', errorMessage: detail })) return detail;
  return `The run retried ${setup.provider} for ${windowText(setup.retry)} without an answer, so it ended. Run it again once ${setup.provider} answers. Last error: ${detail}`;
}

const brief = (text: string) => (text.length > 200 ? `${text.slice(0, 200)}…` : text);

// Conversations sleeping in their retry backoff (a failed try, or a host stop mid-answer), merged into one wait: the earliest
// streak's, a tie to the oldest conversation. `since` is a streak's first failure this process saw.
function parkedWait(setup: ModelFaultSetup) {
  const streaks = new Map<string, { since: number; error?: string }>();
  return (id: string, live: LiveState | undefined) => {
    const g = live?.generation;
    const streak = streaks.get(id);
    if (g?.retry) streaks.set(id, { since: g.attempt === 1 || !streak ? setup.now() : streak.since, error: brief(g.retry.error) });
    else if (!live?.run) streaks.delete(id);
    else if (streak) delete streak.error;
    const [first] = [...streaks].filter(([, s]) => s.error !== undefined).sort(([a, x], [b, y]) => x.since - y.since || a.localeCompare(b, 'en', { numeric: true }));
    modelWait.setDown(first ? { since: first[1].since, ahead: 0, provider: setup.provider, error: first[1].error } : null);
  };
}

// A subagent's child runs in this harness on the same retry policy, so its park is the session's wait too. One commit observer
// sees every child's pi.live, made before or after open; a child with no run left drops out.
async function watchChildren(harness: Harness, root: Conversation, parked: ReturnType<typeof parkedWait>) {
  const seen = new Set<string>();
  harness.observeCommits({
    advance(publication) {
      for (const c of publication.changes) {
        if (c.type !== 'document' || c.record.kind !== LiveDoc.definition.kind || !c.conversationId || c.conversationId === root.id) continue;
        const id = String(c.conversationId);
        const live = c.kind === 'change' ? (c.value as LiveState) : undefined;
        seen.add(id);
        // An observer must not call back into the Session, so the wait is published a microtask later, still in commit order.
        queueMicrotask(() => parked(id, live));
      }
    },
    end: () => undefined,
  });
  let cursor: Cursor | undefined;
  do {
    const page = await harness.commit((tx) => tx.scanConversations({}, 100, cursor), ctx);
    for (const c of page.items) if (c.id !== root.id && !seen.has(String(c.id))) parked(String(c.id), await harness.snapshot(LiveDoc, c.id, ctx));
    cursor = page.next;
  } while (cursor);
}

/** Retries a run a provider abort ended once, and records any other model_error (or a second abort) as a failure so Run again appears.
 * While the run sleeps between tries it shows as waiting on its endpoint. */
export async function watchModelFaults(harness: Harness, root: Conversation, log: (line: string) => void, setup: ModelFaultSetup) {
  await root.commit(async (tx) => { await tx.doc(LiveDoc, root.id); }, ctx);
  const watch = await harness.watchDoc(LiveDoc, root.id, ctx);
  const parked = parkedWait(setup);
  parked(String(root.id), watch?.value);
  await watchChildren(harness, root, parked);
  // A failure present at open is recorded, not retried: a crash after its commit must not leave the session idle with nothing to resume.
  const first = watch?.value?.runFailure;
  let seen = String(first?.taskId ?? '');
  if (first?.reason === 'model_error') await setRow(root, failureText(first.detail, setup), Number(first.taskId) || 0);
  let retrying = false;
  const onChange = async () => {
    const live = watch?.value;
    parked(String(root.id), live);
    const f = live?.runFailure;
    if (!f) {
      if (!live?.run) retrying = false;
      return void (await setRow(root, null, Number(live?.run?.taskId) || 0));
    }
    if (f.reason !== 'model_error' || String(f.taskId) === seen) return;
    seen = String(f.taskId);
    if (isAbort(f.detail) && !retrying && !live?.run) {
      retrying = true;
      return void (await root.submit({ type: 'continue' }, ctx));
    }
    retrying = false;
    await setRow(root, failureText(f.detail, setup), Number(f.taskId) || 0);
  };
  watch?.start(async () => void onChange().catch((e) => log(`session: model fault handling failed: ${e instanceof Error ? e.message : e}`)));
}

/** Reruns the run a model error ended; null when no such failure is recorded. */
export async function resumeModelFault(root: Conversation, harness: Harness): Promise<{ submissionId: string; status: 'placed' } | 'busy' | null> {
  const row = (await harness.snapshot(Failures, root.id, ctx))?.items[MODEL_STEP];
  if (!row?.error) return null;
  // The rerun's start commit clears the row through the watch; a busy conversation is a 409 that says so, not a 500.
  try {
    return { submissionId: String((await root.submit({ type: 'continue' }, ctx)).id), status: 'placed' };
  } catch (e) {
    if (e instanceof ConversationBusy) return 'busy';
    throw e;
  }
}
