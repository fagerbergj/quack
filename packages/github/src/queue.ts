const POLL_MS = 15_000;

/** Where waiting work persists, one JSON item per key; adapters: sqliteWaiting, postgresWaiting. */
export type WaitingStore = {
  /** Stores `item` under `key` and returns the item it replaced. */
  put(key: string, item: string): Promise<string | undefined>;
  list(): Promise<Array<{ key: string; item: string }>>;
  /** Removes `key` only while it still holds `item`, so work that replaced it during an attempt is kept. */
  removeIf(key: string, item: string): Promise<void>;
  close(): Promise<void>;
};

/** Work waiting for a busy session, one item per key, retried until `attempt` says it is done. */
export function waitingQueue<T>(store: WaitingStore, attempt: (key: string, item: T) => Promise<boolean>, pollMs = POLL_MS) {
  const trying = new Set<string>();
  let timer: NodeJS.Timeout | undefined;
  const stop = () => { clearInterval(timer); timer = undefined; };

  async function tick() {
    for (const { key, item } of await store.list()) {
      if (trying.has(key)) continue;
      trying.add(key);
      try {
        if (await attempt(key, JSON.parse(item) as T)) await store.removeIf(key, item);
      } catch (err) {
        console.warn(`github: queued start for ${key} failed, will retry: ${String(err)}`);
      } finally {
        trying.delete(key);
      }
    }
  }
  // An empty sweep is one read; the timer stays armed so work added mid-sweep is never stranded.
  const resume = () => { timer ??= setInterval(() => void tick().catch((err) => console.warn(`github: queue sweep failed: ${String(err)}`)), pollMs).unref(); };

  return {
    /** Returns the item it replaced, if any. */
    async add(key: string, item: T): Promise<T | undefined> {
      const prior = await store.put(key, JSON.stringify(item));
      resume();
      return prior === undefined ? undefined : (JSON.parse(prior) as T);
    },
    resume,
    stop,
    /** One retry pass now, the same one the timer runs. */
    sweep: tick,
  };
}
