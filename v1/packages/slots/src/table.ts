import { DatabaseSync } from 'node:sqlite';

export type AcquireResult = { status: 'granted' } | { status: 'queued'; ahead: number };
export type TableConfig = {
  dbPath: string;
  /** Slots per model endpoint (a provider id); an endpoint with no entry is not gated. */
  limits: Record<string, number>;
  /** A holder or waiter not heard from within this long is gone. */
  ttlMs: number;
  now: () => number;
};

type Entry = { holder: string; state: 'held' | 'waiting'; since: number; expires: number };

// The slot ledger: one row per holder or waiter. Every call first drops expired rows and promotes waiters FIFO,
// so a crashed holder frees its slot by the clock alone and no timer runs in the service.
export class SlotTable {
  readonly #db: DatabaseSync;
  readonly #config: TableConfig;

  constructor(config: TableConfig) {
    this.#config = config;
    this.#db = new DatabaseSync(config.dbPath);
    this.#db.exec(`
      CREATE TABLE IF NOT EXISTS entries (seq INTEGER PRIMARY KEY AUTOINCREMENT, endpoint TEXT NOT NULL, holder TEXT NOT NULL, state TEXT NOT NULL, since INTEGER NOT NULL, expires INTEGER NOT NULL, UNIQUE (endpoint, holder));
      CREATE TABLE IF NOT EXISTS waits (endpoint TEXT NOT NULL, holder TEXT NOT NULL, queued INTEGER NOT NULL, granted INTEGER NOT NULL);
    `);
  }

  close() {
    this.#db.close();
  }

  #sweep(endpoint: string) {
    const now = this.#config.now();
    this.#db.prepare('DELETE FROM entries WHERE endpoint = ? AND expires <= ?').run(endpoint, now);
    const held = (this.#db.prepare("SELECT COUNT(*) AS n FROM entries WHERE endpoint = ? AND state = 'held'").get(endpoint) as { n: number }).n;
    const free = (this.#config.limits[endpoint] ?? 0) - held;
    if (free <= 0) return;
    const next = this.#db.prepare("SELECT holder, since FROM entries WHERE endpoint = ? AND state = 'waiting' ORDER BY seq LIMIT ?").all(endpoint, free) as Array<{ holder: string; since: number }>;
    for (const w of next) {
      this.#db.prepare("UPDATE entries SET state = 'held', expires = ? WHERE endpoint = ? AND holder = ?").run(now + this.#config.ttlMs, endpoint, w.holder);
      this.#db.prepare('INSERT INTO waits (endpoint, holder, queued, granted) VALUES (?, ?, ?, ?)').run(endpoint, w.holder, w.since, now);
    }
  }

  #get(endpoint: string, holder: string) {
    return this.#db.prepare('SELECT * FROM entries WHERE endpoint = ? AND holder = ?').get(endpoint, holder) as Entry | undefined;
  }

  #touch(endpoint: string, holder: string) {
    this.#db.prepare('UPDATE entries SET expires = ? WHERE endpoint = ? AND holder = ?').run(this.#config.now() + this.#config.ttlMs, endpoint, holder);
  }

  /** Idempotent: a holder that already has a place keeps it, so a restarted session never holds two. */
  acquire(endpoint: string, holder: string): AcquireResult {
    if (!(endpoint in this.#config.limits)) return { status: 'granted' };
    this.#sweep(endpoint);
    if (this.#get(endpoint, holder)) this.#touch(endpoint, holder);
    else {
      const now = this.#config.now();
      this.#db.prepare("INSERT INTO entries (endpoint, holder, state, since, expires) VALUES (?, ?, 'waiting', ?, ?)").run(endpoint, holder, now, now + this.#config.ttlMs);
    }
    this.#sweep(endpoint);
    if (this.#get(endpoint, holder)!.state === 'held') return { status: 'granted' };
    const ahead = this.#db.prepare("SELECT COUNT(*) AS n FROM entries WHERE endpoint = ? AND state = 'waiting' AND seq < (SELECT seq FROM entries WHERE endpoint = ? AND holder = ?)").get(endpoint, endpoint, holder) as { n: number };
    return { status: 'queued', ahead: ahead.n };
  }

  /** False when the lease is gone (expired and possibly regranted), so the holder must acquire again. */
  heartbeat(endpoint: string, holder: string): boolean {
    this.#sweep(endpoint);
    if (this.#get(endpoint, holder)?.state !== 'held') return false;
    this.#touch(endpoint, holder);
    return true;
  }

  release(endpoint: string, holder: string) {
    this.#db.prepare('DELETE FROM entries WHERE endpoint = ? AND holder = ?').run(endpoint, holder);
    this.#sweep(endpoint);
  }

  /** Each endpoint's holders, waiters and recent wait durations, for the operate view. */
  snapshot() {
    return Object.entries(this.#config.limits).map(([endpoint, limit]) => {
      this.#sweep(endpoint);
      const rows = this.#db.prepare('SELECT holder, state FROM entries WHERE endpoint = ? ORDER BY seq').all(endpoint) as Array<{ holder: string; state: string }>;
      const waits = this.#db.prepare('SELECT holder, granted - queued AS ms FROM waits WHERE endpoint = ? ORDER BY rowid DESC LIMIT 100').all(endpoint) as Array<{ holder: string; ms: number }>;
      return { endpoint, limit, held: rows.filter((r) => r.state === 'held').map((r) => r.holder), waiting: rows.filter((r) => r.state === 'waiting').map((r) => r.holder), waits };
    });
  }
}
