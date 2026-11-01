import { DatabaseSync } from 'node:sqlite';
import type { Backend, FactRow } from './store.ts';

// Append-only: a released migration is never edited. The applied count lives in user_version.
const MIGRATIONS = [
  `create table index_facts (
     session text not null, id text not null, kind text not null, rev integer not null, at integer not null, data text not null,
     primary key (session, id)
   );
   create index index_facts_kind on index_facts (kind, at desc)`,
];

/** The zero-configuration index: one file in the data dir. */
export function sqliteBackend(path: string): Backend {
  const db = new DatabaseSync(path);
  return {
    async migrate() {
      const { user_version: done } = db.prepare('pragma user_version').get() as { user_version: number };
      for (const [i, sql] of MIGRATIONS.entries()) {
        if (i < done) continue;
        db.exec(`begin; ${sql}; pragma user_version = ${i + 1}; commit`);
      }
    },
    async upsert(session, facts) {
      const stmt = db.prepare(
        `insert into index_facts (session, id, kind, rev, at, data) values (?, ?, ?, ?, ?, ?)
         on conflict (session, id) do update set kind = excluded.kind, rev = excluded.rev, at = excluded.at, data = excluded.data
         where excluded.rev > index_facts.rev`,
      );
      db.exec('begin');
      try {
        for (const f of facts) stmt.run(session, f.id, f.kind, f.rev, f.at, JSON.stringify(f.data));
        db.exec('commit');
      } catch (err) {
        db.exec('rollback');
        throw err;
      }
    },
    async facts(kinds) {
      const rows = db.prepare('select session, id, kind, rev, at, data from index_facts').all() as unknown as Array<Omit<FactRow, 'data'> & { data: string }>;
      return rows.filter((r) => !kinds || kinds.includes(r.kind)).map((r) => ({ ...r, data: JSON.parse(r.data) }));
    },
  };
}
