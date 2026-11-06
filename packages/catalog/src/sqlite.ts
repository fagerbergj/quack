import { DatabaseSync, type StatementSync } from 'node:sqlite';
import type { Param } from './compile.ts';
import { FTS_TABLES, sqlStore, tablesDdl, type Driver, type Sql } from './sql-store.ts';
import type { SessionCatalogStore } from './port.ts';

// Append-only: a released migration is never edited. The applied count lives in user_version.
const MIGRATIONS = [
  [tablesDdl({ text: 'text', int: 'integer' }), ...FTS_TABLES.map((t) => `
    create virtual table catalog_${t}_fts using fts5(fts_text, content='catalog_${t}', content_rowid='rowid', tokenize='unicode61 remove_diacritics 0');
    create trigger catalog_${t}_fts_ai after insert on catalog_${t} begin insert into catalog_${t}_fts (rowid, fts_text) values (new.rowid, new.fts_text); end;
    create trigger catalog_${t}_fts_ad after delete on catalog_${t} begin insert into catalog_${t}_fts (catalog_${t}_fts, rowid, fts_text) values ('delete', old.rowid, old.fts_text); end;
    create trigger catalog_${t}_fts_au after update of fts_text on catalog_${t} begin
      insert into catalog_${t}_fts (catalog_${t}_fts, rowid, fts_text) values ('delete', old.rowid, old.fts_text);
      insert into catalog_${t}_fts (rowid, fts_text) values (new.rowid, new.fts_text);
    end`)].join(';\n'),
];

const REGEX_CACHE_MAX = 256;

/** The zero-configuration catalog: one SQLite file in the distributor's data dir. */
export function sqliteCatalog(path: string, opts: { currency: string }): SessionCatalogStore {
  const db = new DatabaseSync(path);
  db.exec('pragma journal_mode = wal; pragma busy_timeout = 5000');
  const patterns = new Map<string, RegExp>();
  // X REGEXP Y calls regexp(Y, X); dotAll matches PostgreSQL, where . also matches a newline.
  db.function('regexp', { deterministic: true }, (pattern, value) => {
    if (typeof value !== 'string') return 0;
    let re = patterns.get(String(pattern));
    if (!re) {
      if (patterns.size >= REGEX_CACHE_MAX) patterns.clear();
      patterns.set(String(pattern), (re = new RegExp(String(pattern), 's')));
    }
    return re.test(value) ? 1 : 0;
  });
  const statements = new Map<string, StatementSync>();
  const prepared = (text: string) => statements.get(text) ?? statements.set(text, db.prepare(text)).get(text)!;
  const sql: Sql = {
    all: async (text, params = []) => prepared(text).all(...(params as Array<Exclude<Param, undefined>>)) as Record<string, unknown>[],
    run: async (text, params = []) => void prepared(text).run(...(params as Array<Exclude<Param, undefined>>)),
  };
  // One connection: every read and transaction takes its turn, so a read never sees a write's half.
  let queue: Promise<unknown> = Promise.resolve();
  const turn = <T>(fn: () => Promise<T>): Promise<T> => {
    const next = queue.then(fn, fn);
    queue = next.catch(() => {});
    return next;
  };
  const driver: Driver = {
    FALSE: '0',
    regex: (expr) => `${expr} regexp ?`,
    fts: (table, alias, q) => ({
      sql: `${alias}.rowid in (select rowid from catalog_${table}_fts where catalog_${table}_fts match ?)`,
      params: [`"${q.tokens.join(' ')}"${q.prefix ? ' *' : ''}`],
    }),
    tx: (_key, fn) => turn(async () => {
      db.exec('begin immediate');
      try {
        const out = await fn(sql);
        db.exec('commit');
        return out;
      } catch (err) {
        if (db.isTransaction) db.exec('rollback');
        throw err;
      }
    }),
    read: (fn) => turn(() => fn(sql)),
    migrate: () => turn(async () => {
      const { user_version: done } = db.prepare('pragma user_version').get() as { user_version: number };
      const pending = MIGRATIONS.slice(done).map((ddl, i) => `${ddl}; pragma user_version = ${done + i + 1}`);
      if (!pending.length) return;
      try {
        db.exec(`begin; ${pending.join(';\n')}; commit`);
      } catch (err) {
        // A half-open transaction would fail every retry.
        if (db.isTransaction) db.exec('rollback');
        throw err;
      }
    }),
    close: () => turn(async () => db.close()),
  };
  return sqlStore(driver, opts);
}
