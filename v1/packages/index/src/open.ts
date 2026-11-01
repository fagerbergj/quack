import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
import pg from 'pg';
import { sqliteBackend } from './store-sqlite.ts';
import { postgresBackend } from './store-postgres.ts';
import { createStore } from './store.ts';

/** Postgres when QUACK_INDEX_DATABASE_URL is set, otherwise `index.sqlite` in the data dir, so a fresh install needs nothing provisioned. */
export function openStore(env: { QUACK_INDEX_DATABASE_URL?: string; QUACK_DATA_DIR: string }) {
  const url = env.QUACK_INDEX_DATABASE_URL;
  if (!url) {
    mkdirSync(env.QUACK_DATA_DIR, { recursive: true });
    return { store: createStore(sqliteBackend(join(env.QUACK_DATA_DIR, 'index.sqlite'))), close: async () => {} };
  }
  const pool = new pg.Pool({ connectionString: url });
  return { store: createStore(postgresBackend(pool)), close: () => pool.end() };
}
