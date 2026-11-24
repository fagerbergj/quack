import { DatabaseSync } from 'node:sqlite';

/** A holder's place on an endpoint; `expireTime` is absent on an endpoint with no configured capacity, which keeps no row. */
export type Lease = ({ state: 'granted' } | { state: 'queued'; ahead: number }) & { expireTime?: string };
export type LedgerConfig = {
  dbPath: string;
  /** Capacity per model endpoint (a provider id); an endpoint with no entry is not gated. */
  capacity: Record<string, number>;
  /** A holder or waiter not heard from within this long is gone. */
  ttlMs: number;
  now: () => number;
  /** Times held acquires and wakes them at the next expiry; returns the cancel. */
  setTimer: (fn: () => void, ms: number) => () => void;
};

// Grant waits kept per endpoint for the operate view; older ones are dropped as new ones land.
const WAIT_HISTORY = 100;
/** The longest an acquire is held; the client then asks again. */
export const HOLD_MS = 25_000;

type Waiter = { endpoint: string; holder: string; ahead: number; done: (lease: Lease) => void };

type Entry = { seq: number; holder: string; state: 'held' | 'waiting'; since: number; expires: number };

// One row per holder or waiter. Every call first drops expired rows and promotes waiters FIFO,
// so a crashed holder frees its place by the clock alone and no timer runs in the service.
export class LeaseLedger {
  readonly #db: DatabaseSync;
  readonly #config: LedgerConfig;
  readonly #waiters = new Set<Waiter>();
  // Per endpoint with waiters, the timer that wakes them at the next row's expiry: no request would notice it otherwise.
  readonly #expiry = new Map<string, () => void>();

  constructor(config: LedgerConfig) {
    this.#config = config;
    this.#db = new DatabaseSync(config.dbPath);
    // WAL + NORMAL skips the per-commit fsync; a crash can drop the last commits, which lease expiry recovers.
    this.#db.exec(`
      PRAGMA journal_mode = WAL; PRAGMA synchronous = NORMAL;
      CREATE TABLE IF NOT EXISTS entries (seq INTEGER PRIMARY KEY AUTOINCREMENT, endpoint TEXT NOT NULL, holder TEXT NOT NULL, state TEXT NOT NULL, since INTEGER NOT NULL, expires INTEGER NOT NULL, UNIQUE (endpoint, holder));
      CREATE TABLE IF NOT EXISTS waits (endpoint TEXT NOT NULL, holder TEXT NOT NULL, queued INTEGER NOT NULL, granted INTEGER NOT NULL);
    `);
  }

  close() {
    for (const cancel of this.#expiry.values()) cancel();
    for (const w of this.#waiters) w.done({ state: 'queued', ahead: w.ahead });
    this.#db.close();
  }

  gated = (endpoint: string) => Object.hasOwn(this.#config.capacity, endpoint);

  #sweep(endpoint: string) {
    // A held acquire's open request is its heartbeat, so its place never lapses while it waits.
    for (const w of this.#waiters) if (w.endpoint === endpoint) this.#touch(endpoint, w.holder);
    const now = this.#config.now();
    this.#db.prepare('DELETE FROM entries WHERE endpoint = ? AND expires <= ?').run(endpoint, now);
    const held = (this.#db.prepare("SELECT COUNT(*) AS n FROM entries WHERE endpoint = ? AND state = 'held'").get(endpoint) as { n: number }).n;
    const free = (this.#config.capacity[endpoint] ?? 0) - held;
    if (free <= 0) return;
    const next = this.#db.prepare("SELECT holder, since FROM entries WHERE endpoint = ? AND state = 'waiting' ORDER BY seq LIMIT ?").all(endpoint, free) as Array<{ holder: string; since: number }>;
    for (const w of next) {
      this.#db.prepare("UPDATE entries SET state = 'held', expires = ? WHERE endpoint = ? AND holder = ?").run(now + this.#config.ttlMs, endpoint, w.holder);
      this.#db.prepare('INSERT INTO waits (endpoint, holder, queued, granted) VALUES (?, ?, ?, ?)').run(endpoint, w.holder, w.since, now);
    }
    if (next.length) this.#db.prepare('DELETE FROM waits WHERE endpoint = ? AND rowid NOT IN (SELECT rowid FROM waits WHERE endpoint = ? ORDER BY rowid DESC LIMIT ?)').run(endpoint, endpoint, WAIT_HISTORY);
  }

  #get(endpoint: string, holder: string) {
    return this.#db.prepare('SELECT * FROM entries WHERE endpoint = ? AND holder = ?').get(endpoint, holder) as Entry | undefined;
  }

  #touch(endpoint: string, holder: string) {
    this.#db.prepare('UPDATE entries SET expires = ? WHERE endpoint = ? AND holder = ?').run(this.#config.now() + this.#config.ttlMs, endpoint, holder);
  }

  #view(endpoint: string, e: Entry): Lease {
    const expireTime = new Date(e.expires).toISOString();
    if (e.state === 'held') return { state: 'granted', expireTime };
    const ahead = this.#db.prepare("SELECT COUNT(*) AS n FROM entries WHERE endpoint = ? AND state = 'waiting' AND seq < ?").get(endpoint, e.seq) as { n: number };
    return { state: 'queued', ahead: ahead.n, expireTime };
  }

  /** The holder's lease, or undefined when a gated endpoint has no row for it; an ungated endpoint is always granted. */
  lease(endpoint: string, holder: string): Lease | undefined {
    if (!this.gated(endpoint)) return { state: 'granted' };
    this.#sweep(endpoint);
    const e = this.#get(endpoint, holder);
    return e && this.#view(endpoint, e);
  }

  /** Idempotent: a holder that already has a place keeps it, so a restarted session never holds two. */
  acquire(endpoint: string, holder: string): Lease {
    if (!this.gated(endpoint)) return { state: 'granted' };
    this.#sweep(endpoint);
    if (this.#get(endpoint, holder)) this.#touch(endpoint, holder);
    else {
      const now = this.#config.now();
      this.#db.prepare("INSERT INTO entries (endpoint, holder, state, since, expires) VALUES (?, ?, 'waiting', ?, ?)").run(endpoint, holder, now, now + this.#config.ttlMs);
    }
    const lease = this.lease(endpoint, holder)!;
    this.#settle(endpoint);
    return lease;
  }

  /** An acquire held while the holder is queued with `ahead` places before it: it answers on a grant, a move up the queue, or after HOLD_MS.
   * An abort (the client went away) drops the place, so it is never granted to a gone client. */
  wait(endpoint: string, holder: string, ahead: number, signal?: AbortSignal): Promise<Lease> {
    signal?.throwIfAborted();
    const now = this.acquire(endpoint, holder);
    if (now.state === 'granted' || now.ahead !== ahead) return Promise.resolve(now);
    return new Promise((resolve, reject) => {
      const w: Waiter = { endpoint, holder, ahead, done: (lease) => (end(), resolve(lease)) };
      const timeout = this.#config.setTimer(() => w.done(this.acquire(endpoint, holder)), HOLD_MS);
      const gone = () => {
        end();
        // Another request of the same holder (a restarted process) keeps the place.
        if (![...this.#waiters].some((o) => o.endpoint === endpoint && o.holder === holder)) this.release(endpoint, holder);
        reject(signal!.reason);
      };
      const end = () => (this.#waiters.delete(w), timeout(), signal?.removeEventListener('abort', gone));
      this.#waiters.add(w);
      signal?.addEventListener('abort', gone, { once: true });
      this.#armExpiry(endpoint);
    });
  }

  /** Extends a held lease; a queued one is answered unchanged, and undefined means the lease is gone (lapsed, possibly regranted). */
  heartbeat(endpoint: string, holder: string): Lease | undefined {
    const lease = this.lease(endpoint, holder);
    // Its sweep may have expired a holder and promoted a waiter.
    if (this.gated(endpoint)) this.#settle(endpoint);
    if (lease?.state !== 'granted' || !lease.expireTime) return lease;
    this.#touch(endpoint, holder);
    return this.lease(endpoint, holder);
  }

  release(endpoint: string, holder: string) {
    this.#db.prepare('DELETE FROM entries WHERE endpoint = ? AND holder = ?').run(endpoint, holder);
    this.#sweep(endpoint);
    this.#settle(endpoint);
  }

  // Answers each held acquire on the endpoint whose place changed; a waiter whose row is gone waits for its abort or timeout.
  #settle(endpoint: string) {
    for (const w of this.#waiters) {
      const e = w.endpoint === endpoint ? this.#get(endpoint, w.holder) : undefined;
      const lease = e && this.#view(endpoint, e);
      if (lease && (lease.state === 'granted' || lease.ahead !== w.ahead)) w.done(lease);
    }
    this.#armExpiry(endpoint);
  }

  #armExpiry(endpoint: string) {
    this.#expiry.get(endpoint)?.();
    this.#expiry.delete(endpoint);
    if (![...this.#waiters].some((w) => w.endpoint === endpoint)) return;
    const { at } = this.#db.prepare('SELECT MIN(expires) AS at FROM entries WHERE endpoint = ?').get(endpoint) as { at: number | null };
    if (at === null) return;
    this.#expiry.set(endpoint, this.#config.setTimer(() => (this.#expiry.delete(endpoint), this.#sweep(endpoint), this.#settle(endpoint)), Math.max(0, at - this.#config.now())));
  }

  /** Each gated endpoint's holders, waiters and recent wait durations, for the operate view. */
  endpoints() {
    return Object.keys(this.#config.capacity).map((endpoint) => this.endpoint(endpoint)!);
  }

  endpoint(endpoint: string) {
    if (!this.gated(endpoint)) return undefined;
    this.#sweep(endpoint);
    const rows = this.#db.prepare('SELECT holder, state FROM entries WHERE endpoint = ? ORDER BY seq').all(endpoint) as Array<{ holder: string; state: string }>;
    const waits = this.#db.prepare('SELECT holder, granted - queued AS ms FROM waits WHERE endpoint = ? ORDER BY rowid DESC LIMIT ?').all(endpoint, WAIT_HISTORY) as Array<{ holder: string; ms: number }>;
    return { endpoint, capacity: this.#config.capacity[endpoint]!, held: rows.filter((r) => r.state === 'held').map((r) => r.holder), waiting: rows.filter((r) => r.state === 'waiting').map((r) => r.holder), waits };
  }
}
