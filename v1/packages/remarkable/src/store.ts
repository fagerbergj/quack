import { randomUUID } from 'node:crypto';
import { DatabaseSync } from 'node:sqlite';

export type ItemStatus = 'pending' | 'done' | 'failed';
export type BatchItem = { document: string; version: string; status: ItemStatus; error?: string };
export type Batch = { id: string; created: string; status: ItemStatus; items: BatchItem[] };

/** Batch records on the service's volume (RM4). A batch is done when every item is done, failed when all settled and any failed. */
export function openStore(path: string, now: () => Date) {
  const db = new DatabaseSync(path);
  db.exec(`
    CREATE TABLE IF NOT EXISTS batches (id TEXT PRIMARY KEY, created TEXT NOT NULL);
    CREATE TABLE IF NOT EXISTS batch_items (batch TEXT NOT NULL REFERENCES batches(id), document TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', version TEXT NOT NULL, status TEXT NOT NULL, error TEXT, attempt INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (batch, document));
    CREATE TABLE IF NOT EXISTS topics (name TEXT PRIMARY KEY COLLATE NOCASE);
  `);
  // shortcut: a volume written before retries lacks the column; drop this once every deployment has opened the store once.
  if (!(db.prepare('PRAGMA table_info(batch_items)').all() as Array<{ name: string }>).some((c) => c.name === 'attempt')) db.exec('ALTER TABLE batch_items ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0');
  const settle = db.prepare('UPDATE batch_items SET status = ?, error = ? WHERE batch = ? AND document = ?');

  return {
    create(items: Array<{ document: string; name: string; version: string; attempt: number }>): string {
      const id = randomUUID();
      db.prepare('INSERT INTO batches (id, created) VALUES (?, ?)').run(id, now().toISOString());
      const insert = db.prepare("INSERT INTO batch_items (batch, document, name, version, attempt, status) VALUES (?, ?, ?, ?, ?, 'pending')");
      for (const i of items) insert.run(id, i.document, i.name, i.version, i.attempt);
      return id;
    },
    settle(batch: string, document: string, error?: string) {
      settle.run(error === undefined ? 'done' : 'failed', error ?? null, batch, document);
    },
    /** Items still awaiting an answer, for resubmission after a restart. */
    pending: () => db.prepare("SELECT batch, document, name, version, attempt FROM batch_items WHERE status = 'pending' ORDER BY rowid").all() as Array<{ batch: string; document: string; name: string; version: string; attempt: number }>,
    /** The newest attempt at this document version, if any; a retry submits the next attempt number. */
    last: (document: string, version: string) => db.prepare('SELECT status, attempt FROM batch_items WHERE document = ? AND version = ? ORDER BY rowid DESC LIMIT 1').get(document, version) as { status: ItemStatus; attempt: number } | undefined,
    get(id: string): Batch | undefined {
      const b = db.prepare('SELECT id, created FROM batches WHERE id = ?').get(id) as { id: string; created: string } | undefined;
      if (!b) return undefined;
      const rows = db.prepare('SELECT document, version, status, error FROM batch_items WHERE batch = ? ORDER BY rowid').all(id) as Array<{ document: string; version: string; status: ItemStatus; error: string | null }>;
      const items = rows.map((r) => ({ document: r.document, version: r.version, status: r.status, ...(r.error ? { error: r.error } : {}) }));
      const status = items.some((i) => i.status === 'pending') ? 'pending' : items.some((i) => i.status === 'failed') ? 'failed' : 'done';
      return { ...b, status, items };
    },
    /** The filing topics, oldest first; they outlive any session, so the ingest workflow reads them here. */
    topics: () => (db.prepare('SELECT name FROM topics ORDER BY rowid').all() as Array<{ name: string }>).map((r) => r.name),
    addTopic: (name: string) => void db.prepare('INSERT OR IGNORE INTO topics (name) VALUES (?)').run(name),
    close: () => db.close(),
  };
}

export type Store = ReturnType<typeof openStore>;
