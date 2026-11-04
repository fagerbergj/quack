import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
import pg from 'pg';
import type { StoreChoice } from '@quack/sdk';
import { sqliteBackend } from './store-sqlite.ts';
import { postgresBackend } from './store-postgres.ts';
import { createStore } from './store.ts';

/** Postgres when the chosen store is, otherwise `index.sqlite` in the data dir, so a fresh install needs nothing provisioned. */
export function openStore(opts: { dataDir: string; store?: StoreChoice }) {
  if (opts.store?.kind !== 'postgres') {
    mkdirSync(opts.dataDir, { recursive: true });
    const backend = sqliteBackend(join(opts.dataDir, 'index.sqlite'));
    return { store: createStore(backend), close: async () => backend.close() };
  }
  const pool = new pg.Pool({ connectionString: opts.store.url });
  return { store: createStore(postgresBackend(pool)), close: () => pool.end() };
}
