import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
import pg from 'pg';
import { storeFor, type DeploymentFile, type StoreChoice } from '@quack/sdk';
import { createCatalog } from './core.ts';
import { fileJournal } from './journal.ts';
import { postgresCatalog } from './postgres.ts';
import { sqliteCatalog } from './sqlite.ts';

// shortcut: deployment.json v6 names no catalog store, so this reads stores.catalog; v7's core section replaces this one line.
/** The store the deployment file selects for the catalog; none selected is SQLite in the data dir. */
export const catalogStore = (deployment: DeploymentFile, env: NodeJS.ProcessEnv = process.env): StoreChoice => storeFor(deployment, deployment.stores?.catalog && 'catalog', env);

/** The deployment's catalog: Postgres when its store is, else `catalog.sqlite` in the data dir, so a fresh install needs nothing.
 * The action journal is always in the data dir, outside every session's directory. */
export function openCatalog(opts: { dataDir: string; store?: StoreChoice; currency?: string }) {
  const currency = opts.currency ?? 'USD';
  mkdirSync(opts.dataDir, { recursive: true });
  const store = opts.store?.kind === 'postgres'
    ? postgresCatalog(new pg.Pool({ connectionString: opts.store.url }), { currency })
    : sqliteCatalog(join(opts.dataDir, 'catalog.sqlite'), { currency });
  return createCatalog({ store, journal: fileJournal(join(opts.dataDir, 'catalog-journal')) });
}
