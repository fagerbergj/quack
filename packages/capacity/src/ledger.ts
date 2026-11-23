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
};

// Grant waits kept per endpoint for the operate view; older ones are dropped as new ones land.
const WAIT_HISTORY = 100;

type Entry = { seq: number; holder: string; state: 'held' | 'waiting'; since: number; expires: number };

// One row per holder or waiter. Every call first drops expired rows and promotes waiters FIFO,
// so a crashed holder frees its place by the clock alone and no timer runs in the service.
export class LeaseLedger {
  readonly #db: DatabaseSync;
  readonly #config: LedgerConfig;

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
    this.#db.close();
  }

  gated = (endpoint: string) => Object.hasOwn(this.#config.capacity, endpoint);

  #sweep(endpoint: string) {
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
    return this.lease(endpoint, holder)!;
  }

  /** Extends a held lease; a queued one is answered unchanged, and undefined means the lease is gone (lapsed, possibly regranted). */
  heartbeat(endpoint: string, holder: string): Lease | undefined {
    const lease = this.lease(endpoint, holder);
    if (lease?.state !== 'granted' || !lease.expireTime) return lease;
    this.#touch(endpoint, holder);
    return this.lease(endpoint, holder);
  }

  release(endpoint: string, holder: string) {
    this.#db.prepare('DELETE FROM entries WHERE endpoint = ? AND holder = ?').run(endpoint, holder);
    this.#sweep(endpoint);
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
