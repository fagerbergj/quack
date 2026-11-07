import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after } from 'node:test';
import pg from 'pg';
import { createCatalog, fileJournal, postgresCatalog, sqliteCatalog, type SessionCatalogStore } from '../src/index.ts';
import { conformance, T0 } from './conformance.ts';

const url = process.env.QUACK_TEST_DATABASE_URL;
const cleanups: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

async function around(store: SessionCatalogStore, dir: string) {
  let id = 0;
  // A fixed clock and counted ids: the suite never reads the wall clock.
  const catalog = createCatalog({ store, journal: fileJournal(join(dir, 'catalog-journal')), now: () => T0 + 1_000_000, newId: () => `req-${++id}`, log: () => {} });
  return { store, catalog, journalDir: join(dir, 'catalog-journal'), dataDir: dir };
}

async function tempDir() {
  const dir = await mkdtemp(join(tmpdir(), 'quack-catalog-'));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  return dir;
}

conformance('sqlite', async () => {
  const dir = await tempDir();
  const store = sqliteCatalog(join(dir, 'catalog.sqlite'), { currency: 'USD' });
  cleanups.push(() => store.close());
  return around(store, dir);
});

// Each store gets its own schema in the throwaway database, so tests never see each other's rows.
conformance('postgres', async () => {
  const schema = `catalog_${Math.random().toString(36).slice(2, 10)}`;
  const admin = new pg.Pool({ connectionString: url, max: 1 });
  await admin.query(`create schema ${schema}`);
  const pool = new pg.Pool({ connectionString: url, options: `-c search_path=${schema}` });
  const store = postgresCatalog(pool, { currency: 'USD' });
  cleanups.push(async () => { await store.close(); await admin.query(`drop schema ${schema} cascade`); await admin.end(); });
  return around(store, await tempDir());
}, !url && 'QUACK_TEST_DATABASE_URL is not set');
