import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { watchEvents, type Conversation, type Harness } from '@earendil-works/pi-durable';
import { collectFacts, Outbox, type SessionMeta } from './facts.ts';

const ctx = BACKGROUND_CONTEXT;

export type PublisherOptions = { url: string; token: string; meta: SessionMeta; retryMs?: number; now?: () => number };

// Sends a session's unacknowledged facts to the index (A6). The log is the outbox: a fact is unsent while its revision is
// above the acknowledged one in the Outbox document, so a crash, an index outage or a restart loses nothing.
export async function startPublisher(session: { harness: Harness; root: Conversation }, opts: PublisherOptions) {
  const { harness, root } = session;
  const now = opts.now ?? Date.now;
  let dirty = false;
  let timer: NodeJS.Timeout | undefined;

  async function sendOnce(): Promise<void> {
    const acked = (await harness.snapshot(Outbox, root.id, ctx))?.acked ?? {};
    const unsent = (await collectFacts(harness, root, opts.meta, now(), ctx)).filter((f) => (acked[f.id] ?? 0) < f.rev);
    if (!unsent.length) return;
    try {
      const res = await fetch(`${opts.url}/index/facts`, { method: 'POST', headers: { authorization: `Bearer ${opts.token}`, 'content-type': 'application/json' }, body: JSON.stringify({ session: opts.meta.key, facts: unsent }) });
      if (!res.ok) throw new Error(`index answered ${res.status}`);
    } catch {
      timer ??= setTimeout(() => { timer = undefined; void flush(); }, opts.retryMs ?? 5000).unref();
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
  const events = await watchEvents(harness, root.id, ctx);
  events.start(async (batch) => {
    for (const e of batch) {
      if (e.type === 'run_start') await setStatus('running');
      if (e.type === 'run_end') await setStatus('idle');
    }
    await flush();
  });
  await flush();
  return { flush, stop: async () => { clearTimeout(timer); await events.stop(); } };
}
