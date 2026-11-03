import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import type { Conversation, ConversationView, Harness } from '@earendil-works/pi-durable';
import { onGeneration } from '@quack/sdk';
import { collectFacts, Outbox, type SessionMeta, type Spend } from './facts.ts';

const ctx = BACKGROUND_CONTEXT;
const SEND_TIMEOUT_MS = 10_000;
const STOP_WAIT_MS = 5_000;

export type PublisherOptions = { url: string; token: string; meta: SessionMeta; retryMs?: number; pollMs?: number; now?: () => number };

// Sends a session's unacknowledged facts to the index (A6). The log is the outbox: a fact is unsent while its revision is
// above the acknowledged one in the Outbox document, so a crash, an index outage or a restart loses nothing.
export async function startPublisher(session: { harness: Harness; root: Conversation }, opts: PublisherOptions) {
  const { harness, root } = session;
  const now = opts.now ?? Date.now;
  let dirty = false;
  let stopped = false;
  let timer: NodeJS.Timeout | undefined;

  async function sendOnce(): Promise<void> {
    const acked = (await harness.snapshot(Outbox, root.id, ctx))?.acked ?? {};
    const unsent = (await collectFacts(harness, root, opts.meta, now(), ctx)).filter((f) => (acked[f.id] ?? 0) < f.rev);
    if (!unsent.length) return;
    try {
      const res = await fetch(`${opts.url}/index/facts`, { method: 'POST', headers: { authorization: `Bearer ${opts.token}`, 'content-type': 'application/json' }, body: JSON.stringify({ session: opts.meta.key, facts: unsent }), signal: AbortSignal.timeout(SEND_TIMEOUT_MS) });
      if (!res.ok) throw new Error(`index answered ${res.status}`);
    } catch (err) {
      // A 4xx (bad token, bad body) never recovers by retrying, so make it visible.
      if (err instanceof Error && err.message.startsWith('index answered 4')) console.error('index rejected the facts permanently:', err.message);
      if (!stopped) timer ??= setTimeout(() => { timer = undefined; flush().catch(() => {}); }, opts.retryMs ?? 5000).unref();
      return;
    }
    await root.commit(async (tx) => { const doc = await tx.doc(Outbox, root.id); for (const f of unsent) doc.acked[f.id] = f.rev; }, ctx);
  }

  // Single-flight: a call during a send marks the outbox dirty and the loop goes round again; every caller awaits the loop.
  let current: Promise<void> | undefined;
  async function loop(): Promise<void> {
    do { dirty = false; await sendOnce(); } while (dirty);
  }
  function flush(): Promise<void> {
    if (current) { dirty = true; return current; }
    return (current = loop().finally(() => { current = undefined; }));
  }

  const setStatus = (state: 'idle' | 'running') => root.commit(async (tx) => { const s = (await tx.doc(Outbox, root.id)).status; s.state = state; s.seq += 1; s.at = now(); }, ctx);
  // Workflow steps call completeSimple directly (#1979), so their spend is added to the log here, serialized so none is lost.
  let spending: Promise<unknown> = Promise.resolve();
  // A failed commit is final (pi-durable), so retry here rather than drop the spend.
  const record = async (u: Spend, tries = 3): Promise<void> => {
    for (let n = 1; ; n++) {
      try {
        return await root.commit(async (tx) => {
          const doc = await tx.doc(Outbox, root.id);
          doc.direct ??= { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0, at: 0 };
          const d = doc.direct; // re-read: the draft only tracks writes made through its own proxy
          d.at = now();
          d.input += u.input; d.output += u.output; d.cacheRead += u.cacheRead; d.cacheWrite += u.cacheWrite; d.cost += u.cost;
        }, ctx);
      } catch (err) {
        if (n >= tries) throw err;
      }
    }
  };
  const unhear = onGeneration(({ message: { usage: u } }) => {
    if (!u) return;
    spending = spending.then(() => record({ ...u, cost: u.cost?.total ?? 0, at: 0 })).then(() => flush()).catch((e) => console.error('index: direct spend', e));
  });
  // Streamed deltas change only the view's live message, so a flush follows what facts are made of: entries, run state, usage.
  const watch = await root.watch(ctx);
  const runs = (v: ConversationView) => v.docs['pi.live']?.run !== undefined;
  const signature = (v: ConversationView) => JSON.stringify([v.entries.at(-1)?.id, runs(v), v.docs['pi.usage'], (v.docs['pi.live']?.tools as { status: string }[] | undefined)?.map((t) => t.status)]);
  let seen = signature(watch.value);
  let running = runs(watch.value);
  watch.start(async (call) => {
    if (call.kind !== 'value') return;
    const next = signature(call.value);
    if (next === seen) return;
    seen = next;
    if (runs(call.value) !== running) await setStatus((running = !running) ? 'running' : 'idle');
    await flush();
  });
  await flush();
  // A workflow's questions are committed without any run event, so nothing else would announce them.
  const poll = setInterval(() => void flush().catch(() => {}), opts.pollMs ?? 10_000).unref();
  // Waits out an in-flight send (bounded) so its ack commit cannot race session.close().
  async function stop() {
    stopped = true;
    unhear();
    await spending;
    clearInterval(poll);
    clearTimeout(timer);
    await Promise.race([current?.catch(() => {}), new Promise((r) => setTimeout(r, STOP_WAIT_MS).unref())]);
    await watch.stop();
  }
  return { flush, stop };
}
