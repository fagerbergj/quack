import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';

const POLL_MS = 15_000;

/** Work waiting for a busy session, one item per key, retried until `attempt` says it is done; `file` carries it across a restart. */
export function waitingQueue<T>(file: string | undefined, attempt: (key: string, item: T) => Promise<boolean>, pollMs = POLL_MS) {
  let items: { [key: string]: T } = {};
  try {
    if (file) items = JSON.parse(readFileSync(file, 'utf8'));
  } catch {
    // First run or an unreadable file: nothing was waiting.
  }
  const save = () => {
    if (!file) return;
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(`${file}.tmp`, JSON.stringify(items));
    renameSync(`${file}.tmp`, file);
  };
  const trying = new Set<string>();
  let timer: NodeJS.Timeout | undefined;
  const stop = () => { clearInterval(timer); timer = undefined; };

  async function tick() {
    for (const [key, item] of Object.entries(items)) {
      if (trying.has(key)) continue;
      trying.add(key);
      try {
        if (await attempt(key, item) && items[key] === item) { delete items[key]; save(); }
      } catch (err) {
        console.warn(`github: queued start for ${key} failed, will retry: ${String(err)}`);
      } finally {
        trying.delete(key);
      }
    }
    if (!Object.keys(items).length) stop();
  }
  const resume = () => { if (!timer && Object.keys(items).length) timer = setInterval(() => void tick(), pollMs).unref(); };

  return {
    /** Returns the item it replaced, if any. */
    add(key: string, item: T): T | undefined {
      const prior = items[key];
      items[key] = item;
      save();
      resume();
      return prior;
    },
    resume,
    stop,
  };
}
