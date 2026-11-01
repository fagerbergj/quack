import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import type { GithubApp } from './app.ts';

// GitHub never retries an App webhook it failed to connect for; a restart or deploy window loses events.
const EVERY_MS = 2 * 60_000;
const WINDOW_MS = 30 * 60_000; // older failures are stale; a missed review is re-requested by hand
const MAX_REDELIVERIES = 3;
const MAX_PAGES = 10;

type Delivery = { id: number; guid: string; delivered_at: string; redelivery: boolean; status_code: number };

// No connection, or a gateway that found the service down. A 504 may have been processed, so it is not retried.
const neverReached = (code: number) => code === 0 || code === 502 || code === 503;
const at = (d: Delivery) => Date.parse(d.delivered_at);

/** Oldest first: one delivery per event whose attempts all missed the service, with fewer than MAX_REDELIVERIES retries. */
export function failedDeliveries(ds: Delivery[], sent: Map<string, number> = new Map()): Delivery[] {
  const reached = new Set<string>();
  const latest = new Map<string, Delivery>();
  const tries = new Map<string, number>();
  for (const d of ds) {
    if (!neverReached(d.status_code)) reached.add(d.guid);
    if (d.redelivery) tries.set(d.guid, (tries.get(d.guid) ?? 0) + 1);
    const l = latest.get(d.guid);
    if (!l || at(d) > at(l)) latest.set(d.guid, d);
  }
  return [...latest.values()]
    .filter((d) => !reached.has(d.guid) && Math.max(tries.get(d.guid) ?? 0, sent.get(d.guid) ?? 0) < MAX_REDELIVERIES)
    .sort((a, b) => at(a) - at(b));
}

async function deliveriesSince(app: GithubApp, since: number): Promise<Delivery[]> {
  const out: Delivery[] = [];
  let path = '/app/hook/deliveries?per_page=100';
  for (let page = 0; page < MAX_PAGES; page++) {
    const { json, link } = await app.asApp('GET', path);
    const batch = (json ?? []) as Delivery[];
    out.push(...batch.filter((d) => at(d) > since));
    const next = /<[^>]*?(\/app\/hook\/deliveries\?[^>]*)>;\s*rel="next"/.exec(link);
    if (!next || batch.some((d) => at(d) <= since)) break;
    path = next[1];
  }
  return out;
}

export type Store = { read(): number | undefined; write(mark: number): void };

export function fileStore(file: string): Store {
  return {
    read() {
      try {
        return (JSON.parse(readFileSync(file, 'utf8')) as { since: number }).since;
      } catch {
        return undefined;
      }
    },
    write(mark) {
      mkdirSync(dirname(file), { recursive: true });
      writeFileSync(file, JSON.stringify({ since: mark }));
    },
  };
}

/** Re-sends deliveries newer than the stored high-water mark that never reached the service; returns how many. */
export async function redeliverFailed(app: GithubApp, store: Store, o: { now?: () => number; gapMs?: number; sent?: Map<string, number> } = {}): Promise<number> {
  const sent = o.sent ?? new Map<string, number>();
  const since = Math.max(store.read() ?? 0, (o.now ?? Date.now)() - WINDOW_MS);
  const ds = await deliveriesSince(app, since);
  let mark = Math.max(since, ...ds.map(at));
  let n = 0;
  for (const d of failedDeliveries(ds, sent)) {
    try {
      await app.asApp('POST', `/app/hook/deliveries/${d.id}/attempts`);
    } catch (err) {
      store.write(at(d) - 1); // hold the mark before the delivery that failed to re-send, so the next sweep retries it
      throw err;
    }
    sent.set(d.guid, (sent.get(d.guid) ?? 0) + 1);
    n++;
    await sleep(o.gapMs ?? 2000);
  }
  store.write(mark);
  return n;
}

/** Sweeps now and then every two minutes; returns a stop function. */
export function startRedelivery(app: GithubApp, file: string, log: Pick<Console, 'warn' | 'info'> = console): () => void {
  const store = fileStore(file);
  const sent = new Map<string, number>();
  const sweep = () =>
    redeliverFailed(app, store, { sent })
      .then((n) => n > 0 && log.info(`github: redelivered ${n} webhook deliveries that never reached the service`))
      .catch((err) => log.warn(`github: webhook redelivery sweep failed: ${String(err)}`));
  void sweep();
  const timer = setInterval(sweep, EVERY_MS);
  timer.unref();
  return () => clearInterval(timer);
}
