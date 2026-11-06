/** A batch item still awaiting its session's answer. */
export type QueuedItem = { batch: string; document: string; name: string; version: string; attempt: number };

/** The queue's port onto the batch store: the unanswered items, oldest first. Settling an item removes it. */
export type QueueStore = { pending(): QueuedItem[] };

// Each running document holds a session, its renderer and a held request, so selecting 100 notebooks must not start 100.
export const INGEST_CONCURRENCY = 3;

/** At most `limit` submissions at once, oldest first; the store's unanswered rows are the queue, so it survives a restart. */
export function createIngestQueue(store: QueueStore, run: (item: QueuedItem) => Promise<void>, limit = INGEST_CONCURRENCY, log: (line: string) => void = console.error) {
  const running = new Map<string, Promise<void>>();
  // An item whose answer could not be recorded stays pending for the next start instead of being resubmitted in a loop.
  const stuck = new Set<string>();
  let stopped = false;
  const keyOf = (i: QueuedItem) => `${i.batch}\n${i.document}`;

  function pump() {
    if (stopped) return;
    for (const item of store.pending()) {
      if (running.size >= limit) return;
      const key = keyOf(item);
      if (running.has(key) || stuck.has(key)) continue;
      const done = run(item).catch((err: Error) => {
        stuck.add(key);
        log(`remarkable: ingest of ${item.document} in batch ${item.batch} stays pending: ${err.message}`);
      }).finally(() => {
        running.delete(key);
        pump();
      });
      running.set(key, done);
    }
  }

  return {
    /** Starts queued items while there is room; call it after adding items. */
    pump,
    /** Resolves when nothing is running; with the queue live, that is when every pending item has been answered. */
    async drained() {
      while (running.size) await Promise.all(running.values());
    },
    stop() {
      stopped = true;
    },
  };
}
