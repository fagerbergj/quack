import { DatabaseSync } from 'node:sqlite';
import type { Principal } from '@quack/sdk';
import { MAX_FRAME } from '@quack/session';

/** An input a page sent while its session had no process, kept until the session answers for its requestId. */
export type OutboxRow = { requestId: string; frame: string; text: string; by?: Principal };

export type Outbox = {
  /** Commits the row before it returns, or answers the limit it would pass; a requestId already queued is kept once. */
  add(key: string, row: OutboxRow): string | undefined;
  rows(key: string): OutboxRow[];
  /** How many rows the session has waiting, without reading them. */
  pending(key: string): number;
  remove(key: string, requestId: string): boolean;
  keys(): string[];
};

/** Per session: a burst of follow-ups, and room for two inputs at the page frame budget. */
export const OUTBOX_LIMITS = { inputs: 20, bytes: 2 * MAX_FRAME };

/** The outbox in its own SQLite file beside the session dirs; ':memory:' keeps it for this process only. */
export function openOutbox(path: string, limits = OUTBOX_LIMITS): Outbox {
  const db = new DatabaseSync(path);
  db.exec('pragma journal_mode = wal; create table if not exists outbox (seq integer primary key autoincrement, key text not null, request_id text not null, frame text not null, bytes integer not null, text text not null, by text, unique (key, request_id))');
  const used = db.prepare('select count(*) as n, coalesce(sum(bytes), 0) as bytes from outbox where key = ?');
  const insert = db.prepare('insert into outbox (key, request_id, frame, bytes, text, by) values (?, ?, ?, ?, ?, ?) on conflict do nothing');
  const list = db.prepare('select request_id, frame, text, by from outbox where key = ? order by seq');
  const drop = db.prepare('delete from outbox where key = ? and request_id = ?');
  const keys = db.prepare('select distinct key from outbox');
  const usage = (key: string) => used.get(key) as { n: number; bytes: number };
  return {
    add(key, row) {
      const { n, bytes } = usage(key);
      const size = Buffer.byteLength(row.frame);
      if (n >= limits.inputs) return `at most ${limits.inputs} messages can wait for a session to start`;
      if (bytes + size > limits.bytes) return `messages waiting for a session to start are limited to ${Math.floor(limits.bytes / 2 ** 20)} MiB in all`;
      insert.run(key, row.requestId, row.frame, size, row.text, row.by ? JSON.stringify(row.by) : null);
    },
    rows: (key) => (list.all(key) as Array<{ request_id: string; frame: string; text: string; by: string | null }>).map((r) => ({ requestId: r.request_id, frame: r.frame, text: r.text, ...(r.by && { by: JSON.parse(r.by) as Principal }) })),
    pending: (key) => usage(key).n,
    remove: (key, requestId) => Number(drop.run(key, requestId).changes) > 0,
    keys: () => (keys.all() as Array<{ key: string }>).map((r) => r.key),
  };
}
