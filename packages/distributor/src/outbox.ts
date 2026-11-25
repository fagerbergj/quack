import { DatabaseSync } from 'node:sqlite';
import type { Principal } from '@quack/sdk';
import { MAX_FRAME } from '@quack/session';

/** An input a page sent while its session had no process, kept until the session answers for its requestId; `failed` once delivery gave up. */
export type OutboxRow = { requestId: string; frame: string; text: string; by?: Principal; failed?: string };

export type Outbox = {
  /** Commits the row before it returns, or answers the limit it would pass; a requestId already queued is kept once. */
  add(key: string, row: OutboxRow): string | undefined;
  /** Every row of the session in the order sent, failed ones included. */
  rows(key: string): OutboxRow[];
  /** How many rows still wait for delivery, without reading them. */
  pending(key: string): number;
  remove(key: string, requestId: string): boolean;
  /** Marks every row still waiting as failed, with the reason pages show. */
  fail(key: string, reason: string): void;
  /** Puts a failed row back in line; false when there is no such failed row. */
  retry(key: string, requestId: string): boolean;
  /** Discards the session's rows; answers how many there were. */
  clear(key: string): number;
  /** Sessions with rows still waiting for delivery. */
  keys(): string[];
};

/** Per session: a burst of follow-ups, and room for two inputs at the page frame budget. */
export const OUTBOX_LIMITS = { inputs: 20, bytes: 2 * MAX_FRAME };

type Row = { request_id: string; frame: string; text: string; by: string | null; failed: string | null };

/** The outbox in its own SQLite file beside the session dirs; ':memory:' keeps it for this process only. */
export function openOutbox(path: string, limits = OUTBOX_LIMITS): Outbox {
  const db = new DatabaseSync(path);
  db.exec('pragma journal_mode = wal; create table if not exists outbox (seq integer primary key autoincrement, key text not null, request_id text not null, frame text not null, bytes integer not null, text text not null, by text, failed text, unique (key, request_id))');
  const used = db.prepare('select count(*) as n, coalesce(sum(bytes), 0) as bytes from outbox where key = ?');
  const insert = db.prepare('insert into outbox (key, request_id, frame, bytes, text, by) values (?, ?, ?, ?, ?, ?) on conflict do nothing');
  const list = db.prepare('select request_id, frame, text, by, failed from outbox where key = ? order by seq');
  const waiting = db.prepare('select count(*) as n from outbox where key = ? and failed is null');
  const drop = db.prepare('delete from outbox where key = ? and request_id = ?');
  const failAll = db.prepare('update outbox set failed = ? where key = ? and failed is null');
  const unfail = db.prepare('update outbox set failed = null where key = ? and request_id = ? and failed is not null');
  const dropAll = db.prepare('delete from outbox where key = ?');
  const keys = db.prepare('select distinct key from outbox where failed is null');
  const changes = (r: { changes: number | bigint }) => Number(r.changes);
  return {
    add(key, row) {
      const { n, bytes } = used.get(key) as { n: number; bytes: number };
      const size = Buffer.byteLength(row.frame);
      if (n >= limits.inputs) return `at most ${limits.inputs} messages can wait for a session to start`;
      if (bytes + size > limits.bytes) return `messages waiting for a session to start are limited to ${Math.floor(limits.bytes / 2 ** 20)} MiB in all`;
      insert.run(key, row.requestId, row.frame, size, row.text, row.by ? JSON.stringify(row.by) : null);
    },
    rows: (key) => (list.all(key) as Row[]).map((r) => ({ requestId: r.request_id, frame: r.frame, text: r.text, ...(r.by && { by: JSON.parse(r.by) as Principal }), ...(r.failed !== null && { failed: r.failed }) })),
    pending: (key) => (waiting.get(key) as { n: number }).n,
    remove: (key, requestId) => changes(drop.run(key, requestId)) > 0,
    fail: (key, reason) => void failAll.run(reason, key),
    retry: (key, requestId) => changes(unfail.run(key, requestId)) > 0,
    clear: (key) => changes(dropAll.run(key)),
    keys: () => (keys.all() as Array<{ key: string }>).map((r) => r.key),
  };
}
