import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import type { Conversation, ConversationView, Harness } from '@earendil-works/pi-durable';
import { Approvals, Failures, failedRows, onGeneration } from '@quack/sdk';
import { append, LifecycleStream, openRun, readStream, type PriceOf, type SessionFacts, type Spend, type WireEvent } from './lifecycle.ts';

const ctx = BACKGROUND_CONTEXT;
const SEND_TIMEOUT_MS = 10_000;
const STOP_WAIT_MS = 5_000;
const RETRY_MAX_MS = 60_000;
/** The channel's batch caps: 500 events, and a body under 1 MiB. */
export const MAX_BATCH_EVENTS = 500;
export const MAX_BODY_BYTES = 1024 * 1024;

/** Whether the conversation has a run going. */
export const runsIn = (v: ConversationView) => v.docs['pi.live']?.run !== undefined;
/** What a flush depends on in the conversation view: its last entry, run state, usage and tool states, never streamed deltas. */
export const viewSignature = (v: ConversationView) => JSON.stringify([v.entries.at(-1)?.id, runsIn(v), v.docs['pi.usage'], (v.docs['pi.live']?.tools as { status: string }[] | undefined)?.map((t) => t.status)]);

export type SetTimer = (fn: () => void, ms: number) => () => void;
/** A timer that never holds the process open; returns its cancel. */
export const unrefTimer: SetTimer = (fn, ms) => {
  const t = setTimeout(fn, ms);
  t.unref();
  return () => clearTimeout(t);
};

export type SenderOptions = {
  /** The distributor's session listener and this launch's credential. */
  url: string; token: string; key: string;
  facts: () => Promise<SessionFacts>;
  priceOf?: PriceOf;
  /** The text of the session's last assistant message, read when a run ends. */
  lastMessage?: () => Promise<string | undefined>;
  fetch?: typeof fetch; setTimer?: SetTimer; now?: () => number; log?: (line: string) => void;
  retryMs?: number; pollMs?: number;
};

const bytes = (s: string) => Buffer.byteLength(s);

/** The leading events that fit one request; at least one, so an oversized event is the server's 413 to report. */
export function batchOf(events: WireEvent[], limit = MAX_BODY_BYTES): WireEvent[] {
  let size = bytes('{"events":[]}');
  const out: WireEvent[] = [];
  for (const e of events) {
    size += bytes(JSON.stringify(e)) + (out.length ? 1 : 0);
    if (out.length && (size > limit || out.length >= MAX_BATCH_EVENTS)) break;
    out.push(e);
  }
  return out;
}

type Outcome = { acked: number } | { retry: true } | { split: true } | { refused: string };

async function post(opts: SenderOptions, batch: WireEvent[]): Promise<Outcome> {
  let res: Response;
  try {
    res = await (opts.fetch ?? fetch)(`${opts.url}/sessions/${encodeURIComponent(opts.key)}/events`, {
      method: 'POST', headers: { authorization: `Bearer ${opts.token}`, 'content-type': 'application/json' }, body: JSON.stringify({ events: batch }), signal: AbortSignal.timeout(SEND_TIMEOUT_MS),
    });
  } catch {
    return { retry: true };
  }
  const body = (await res.json().catch(() => ({}))) as { acked?: unknown; detail?: unknown };
  if ((res.status === 200 || res.status === 409) && Number.isSafeInteger(body.acked)) return { acked: body.acked as number };
  if (res.status === 413) return { split: true };
  // 400, 401 and 404 never recover by sending the same body again.
  if (res.status === 400 || res.status === 401 || res.status === 404) return { refused: `${res.status} ${String(body.detail ?? '')}` };
  return { retry: true };
}

// Sends the session's lifecycle events to the distributor's catalog. The log is the outbox: every revision is appended to
// the stream document before it is sent, and the unsent events are those past the acknowledged seq.
export async function startLifecycleSender(session: { harness: Harness; root: Conversation }, opts: SenderOptions) {
  const { harness, root } = session;
  const now = opts.now ?? Date.now;
  const setTimer = opts.setTimer ?? unrefTimer;
  const log = opts.log ?? ((l: string) => console.error(l));
  const retryMs = opts.retryMs ?? 5000;
  let stopped = false;
  let cancelRetry: (() => void) | undefined;
  let failures = 0;
  let refused: string | undefined;

  const commit = (fn: (doc: Parameters<Parameters<Conversation['commit']>[0]>[0]) => Promise<void>) => root.commit(fn, ctx);

  async function record() {
    const { fresh, legacyDirect } = await readStream(harness, root, await opts.facts(), opts.priceOf ?? (() => undefined), ctx);
    if (!fresh.length && !legacyDirect) return;
    await commit(async (tx) => {
      const doc = await tx.doc(LifecycleStream, root.id);
      if (legacyDirect && !doc.direct) doc.direct = legacyDirect;
      append(doc, fresh);
    });
  }

  function retryLater() {
    if (stopped || cancelRetry) return;
    const delay = Math.min(retryMs * 2 ** failures++, RETRY_MAX_MS);
    cancelRetry = setTimer(() => { cancelRetry = undefined; void flush().catch(() => {}); }, delay);
  }

  // A lower ack than ours means the catalog was reset: resume from it, which re-sends from there.
  async function send(): Promise<void> {
    let limit = MAX_BODY_BYTES;
    for (;;) {
      const doc = await harness.snapshot(LifecycleStream, root.id, ctx);
      const batch = batchOf((doc?.events ?? []).filter((e) => e.seq > doc!.acked), limit);
      if (!batch.length) return;
      const body = JSON.stringify(batch);
      if (body === refused) return;
      const out = await post(opts, batch);
      if ('retry' in out) return retryLater();
      if ('refused' in out) return void (refused = body, log(`lifecycle: the distributor refused events ${batch[0]!.seq}-${batch.at(-1)!.seq} permanently: ${out.refused}`));
      if ('split' in out) {
        if (batch.length === 1) return void (refused = body, log(`lifecycle: event ${batch[0]!.seq} is over the channel's body limit`));
        limit = Math.floor(bytes(JSON.stringify({ events: batch })) / 2);
        continue;
      }
      failures = 0;
      // An ack that does not move is a refusal the catalog holds (spend going backwards, a tombstoned key): wait for news.
      if (out.acked === doc!.acked) return;
      await commit(async (tx) => { (await tx.doc(LifecycleStream, root.id)).acked = out.acked; });
    }
  }

  // Single-flight: a call during a flush marks it dirty and the loop goes round again; every caller awaits the loop.
  let dirty = false;
  let current: Promise<void> | undefined;
  async function loop() {
    do { dirty = false; await record(); await send(); } while (dirty && !stopped);
  }
  function flush(): Promise<void> {
    if (current) { dirty = true; return current; }
    return (current = loop().finally(() => { current = undefined; }));
  }

  async function runChanged(running: boolean, booting = false) {
    const at = now();
    const lastMessage = running ? null : ((await opts.lastMessage?.().catch(() => undefined)) ?? null);
    await commit(async (tx) => {
      const doc = await tx.doc(LifecycleStream, root.id);
      const open = doc.runs.at(-1)?.endedAt === null ? doc.runs.at(-1) : undefined;
      if (running && !open) openRun(doc, at);
      if (running && open && booting) open.resumes = [...(open.resumes ?? []), at];
      if (!running && !open && booting) doc.boots = [...(doc.boots ?? []), at];
      if (running || !open) return;
      // An approval still without a verdict when its run ends is cancelled: nobody waits on it.
      const approvals = (await tx.doc(Approvals, root.id)).items;
      Object.assign(open, { endedAt: at, lastMessage, failedSteps: JSON.parse(JSON.stringify(failedRows(await tx.doc(Failures, root.id)))), cancelled: Object.keys(approvals).filter((id) => !approvals[id]!.verdict) });
    });
  }

  // The first derivation is recorded before a run record or spend can create the stream, so it matches the backfill's.
  await record();
  // Workflow steps call completeSimple directly, so their spend is added to the log here, serialized so none is lost.
  let spending: Promise<unknown> = Promise.resolve();
  const unhear = onGeneration(({ message: { usage: u } }) => {
    if (!u) return;
    const add = async () => commit(async (tx) => {
      const doc = await tx.doc(LifecycleStream, root.id);
      doc.direct ??= { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0, at: 0 };
      const d: Spend = doc.direct; // re-read: the draft only tracks writes made through its own proxy
      d.at = now();
      d.input += u.input; d.output += u.output; d.cacheRead += u.cacheRead; d.cacheWrite += u.cacheWrite; d.cost += u.cost?.total ?? 0;
    });
    spending = spending.then(add).then(() => flush()).catch((e) => log(`lifecycle: direct spend ${String(e)}`));
  });

  // Streamed deltas change only the live message, so a flush follows what events are made of: entries, run state, usage.
  const watch = await root.watch(ctx);
  const runs = runsIn;
  const signatureOf = viewSignature;
  let seen = signatureOf(watch.value);
  let running = runs(watch.value);
  // A run cut by a restart is closed now; a run still going on open is recorded, or reported running again.
  await runChanged(running, true);
  watch.start(async (call) => {
    if (call.kind !== 'value') return;
    const next = signatureOf(call.value);
    if (next === seen) return;
    seen = next;
    if (runs(call.value) !== running) await runChanged((running = !running));
    await flush();
  });
  await flush();
  // A workflow's questions are committed without any run event, so nothing else would announce them.
  let cancelPoll = () => {};
  const poll = () => { cancelPoll = setTimer(() => { void flush().catch(() => {}).finally(() => !stopped && poll()); }, opts.pollMs ?? 10_000); };
  poll();

  // Waits out an in-flight send (bounded) so its ack commit cannot race session.close().
  async function stop() {
    stopped = true;
    unhear();
    await spending;
    cancelPoll();
    cancelRetry?.();
    await Promise.race([current?.catch(() => {}), new Promise((r) => setTimer(() => r(undefined), STOP_WAIT_MS))]);
    await watch.stop();
  }
  return { flush, stop };
}
