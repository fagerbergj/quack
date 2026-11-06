import { existsSync, readFileSync, renameSync } from 'node:fs';
import { DatabaseSync } from 'node:sqlite';
import pg from 'pg';
import type { WaitingStore } from './queue.ts';

/** The waiting store in a SQLite file; ':memory:' keeps it for the process only. */
export function sqliteWaiting(path: string): WaitingStore {
  const db = new DatabaseSync(path);
  db.exec('CREATE TABLE IF NOT EXISTS github_waiting (key TEXT PRIMARY KEY, item TEXT NOT NULL)');
  const read = db.prepare('SELECT item FROM github_waiting WHERE key = ?');
  const upsert = db.prepare('INSERT INTO github_waiting (key, item) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET item = excluded.item');
  return {
    // Synchronous statements with no await between them, so the read and the write are one step for this process.
    async put(key, item) {
      const prior = (read.get(key) as { item: string } | undefined)?.item;
      upsert.run(key, item);
      return prior;
    },
    async list() {
      return (db.prepare('SELECT key, item FROM github_waiting ORDER BY key').all() as Array<{ key: string; item: string }>).map(({ key, item }) => ({ key, item }));
    },
    async removeIf(key, item) {
      db.prepare('DELETE FROM github_waiting WHERE key = ? AND item = ?').run(key, item);
    },
    async close() {
      db.close();
    },
  };
}

/** The waiting store in PostgreSQL; the table is created on first use. */
export function postgresWaiting(pool: pg.Pool): WaitingStore {
  let ready: Promise<unknown> | undefined;
  const ensure = () => (ready ??= pool.query('create table if not exists github_waiting (key text primary key, item text not null)').catch((err) => { ready = undefined; throw err; }));
  const q = async <R extends pg.QueryResultRow>(sql: string, args: unknown[] = []) => {
    await ensure();
    return pool.query<R>(sql, args);
  };
  return {
    async put(key, item) {
      await ensure();
      const client = await pool.connect();
      try {
        await client.query('begin');
        const prior = (await client.query<{ item: string }>('select item from github_waiting where key = $1 for update', [key])).rows[0]?.item;
        await client.query('insert into github_waiting (key, item) values ($1, $2) on conflict (key) do update set item = excluded.item', [key, item]);
        await client.query('commit');
        return prior;
      } catch (err) {
        await client.query('rollback').catch(() => {});
        throw err;
      } finally {
        client.release();
      }
    },
    async list() {
      return (await q<{ key: string; item: string }>('select key, item from github_waiting order by key')).rows;
    },
    async removeIf(key, item) {
      await q('delete from github_waiting where key = $1 and item = $2', [key, item]);
    },
    async close() {
      await pool.end();
    },
  };
}

/** Moves `/fix`es an older release kept in a JSON file into the store, once; the file is renamed so it is not read again. */
export async function importWaitingFile(store: WaitingStore, file: string): Promise<void> {
  if (!existsSync(file)) return;
  let items: Record<string, unknown> = {};
  try {
    items = JSON.parse(readFileSync(file, 'utf8'));
  } catch (err) {
    console.warn(`github: ${file} is unreadable, so no queued /fix is imported from it: ${String(err)}`);
  }
  for (const [key, item] of Object.entries(items)) await store.put(key, JSON.stringify(item));
  renameSync(file, `${file}.imported`);
}
