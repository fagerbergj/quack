const realSleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/** Calls fn until it resolves, up to `attempts` times, sleeping `delayMs` between tries. */
export async function retry<T>(fn: () => Promise<T>, o: { attempts: number; delayMs: number; sleep?: (ms: number) => Promise<void> }): Promise<T> {
  const sleep = o.sleep ?? realSleep;
  for (let i = 1; ; i++) {
    try {
      return await fn();
    } catch (err) {
      if (i >= o.attempts) throw err;
      await sleep(o.delayMs);
    }
  }
}
