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
    CREATE TABLE IF NOT EXISTS batch_items (batch TEXT NOT NULL REFERENCES batches(id), document TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', version TEXT NOT NULL, status TEXT NOT NULL, error TEXT, PRIMARY KEY (batch, document));
  `);
  const settle = db.prepare('UPDATE batch_items SET status = ?, error = ? WHERE batch = ? AND document = ?');

  return {
    create(items: Array<{ document: string; name: string; version: string }>): string {
      const id = randomUUID();
      db.prepare('INSERT INTO batches (id, created) VALUES (?, ?)').run(id, now().toISOString());
      const insert = db.prepare("INSERT INTO batch_items (batch, document, name, version, status) VALUES (?, ?, ?, ?, 'pending')");
      for (const i of items) insert.run(id, i.document, i.name, i.version);
      return id;
    },
    settle(batch: string, document: string, error?: string) {
      settle.run(error === undefined ? 'done' : 'failed', error ?? null, batch, document);
    },
    /** Items still awaiting an answer, for resubmission after a restart. */
    pending: () => db.prepare("SELECT batch, document, name, version FROM batch_items WHERE status = 'pending' ORDER BY rowid").all() as Array<{ batch: string; document: string; name: string; version: string }>,
    get(id: string): Batch | undefined {
      const b = db.prepare('SELECT id, created FROM batches WHERE id = ?').get(id) as { id: string; created: string } | undefined;
      if (!b) return undefined;
      const rows = db.prepare('SELECT document, version, status, error FROM batch_items WHERE batch = ? ORDER BY rowid').all(id) as Array<{ document: string; version: string; status: ItemStatus; error: string | null }>;
      const items = rows.map((r) => ({ document: r.document, version: r.version, status: r.status, ...(r.error ? { error: r.error } : {}) }));
      const status = items.some((i) => i.status === 'pending') ? 'pending' : items.some((i) => i.status === 'failed') ? 'failed' : 'done';
      return { ...b, status, items };
    },
    close: () => db.close(),
  };
}

export type Store = ReturnType<typeof openStore>;
