import pg from 'pg';
import type { Param } from './compile.ts';
import { FTS_TABLES, sqlStore, tablesDdl, type Driver, type Sql } from './sql-store.ts';
import type { SessionCatalogStore } from './port.ts';

// Append-only: a released migration is never edited. "C" collation orders text by code point, as SQLite does.
const MIGRATIONS = [
  [tablesDdl({ text: 'text collate "C"', int: 'bigint' }), ...FTS_TABLES.map((t) => `
    alter table catalog_${t} add column fts tsvector generated always as (to_tsvector('simple', fts_text)) stored;
    create index catalog_${t}_fts on catalog_${t} using gin (fts)`)].join(';\n'),
];

const MIGRATION_LOCK = 7462;

// The shared SQL writes ? placeholders; int8 travels as text so micros past 2^53 stay exact.
const numbered = (text: string) => { let n = 0; return text.replace(/\?/g, () => `$${++n}`); };
const values = (params: Param[]) => params.map((p) => (typeof p === 'bigint' ? p.toString() : p));
const over = (c: pg.Pool | pg.PoolClient): Sql => ({
  all: async (text, params = []) => (await c.query(numbered(text), values(params))).rows,
  run: async (text, params = []) => void (await c.query(numbered(text), values(params))),
});

/** The catalog on PostgreSQL; a session's writes serialize on an advisory lock keyed by the session. */
export function postgresCatalog(pool: pg.Pool, opts: { currency: string }): SessionCatalogStore {
  async function inTx<T>(fn: (client: pg.PoolClient) => Promise<T>): Promise<T> {
    const client = await pool.connect();
    try {
      await client.query('begin');
      const out = await fn(client);
      await client.query('commit');
      return out;
    } catch (err) {
      await client.query('rollback').catch(() => {});
      throw err;
    } finally {
      client.release();
    }
  }
  const driver: Driver = {
    FALSE: 'false',
    regex: (expr) => `${expr} ~ ?`,
    fts: (_table, alias, q) => ({ sql: `${alias}.fts @@ to_tsquery('simple', ?)`, params: [`${q.tokens.join(' <-> ')}${q.prefix ? ':*' : ''}`] }),
    tx: (key, fn) => inTx(async (client) => {
      await client.query('select pg_advisory_xact_lock(hashtextextended($1, 7461))', [key]);
      return fn(over(client));
    }),
    read: (fn) => fn(over(pool)),
    migrate: () => inTx(async (client) => {
      await client.query(`select pg_advisory_xact_lock(${MIGRATION_LOCK})`);
      await client.query('create table if not exists catalog_migrations (version int primary key)');
      const done = new Set((await client.query<{ version: number }>('select version from catalog_migrations')).rows.map((r) => r.version));
      for (const [i, ddl] of MIGRATIONS.entries()) {
        if (done.has(i + 1)) continue;
        await client.query(ddl);
        await client.query('insert into catalog_migrations (version) values ($1)', [i + 1]);
      }
    }),
    close: () => pool.end(),
  };
  return sqlStore(driver, opts);
}
