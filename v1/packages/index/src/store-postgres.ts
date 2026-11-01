import pg from 'pg';
import type { Backend, FactRow } from './store.ts';

// Append-only: a released migration is never edited.
const MIGRATIONS = [
  `create table index_facts (
     session text not null, id text not null, kind text not null, rev bigint not null, at bigint not null, data jsonb not null,
     primary key (session, id)
   );
   create index index_facts_kind on index_facts (kind, at desc)`,
];

export function postgresBackend(pool: pg.Pool): Backend {
  return {
    close: () => void pool.end(),
    async migrate() {
      const client = await pool.connect();
      try {
        await client.query('begin');
        await client.query('select pg_advisory_xact_lock(7461)');
        await client.query('create table if not exists index_migrations (version int primary key)');
        const done = new Set((await client.query<{ version: number }>('select version from index_migrations')).rows.map((r) => r.version));
        for (const [i, sql] of MIGRATIONS.entries()) {
          if (done.has(i + 1)) continue;
          await client.query(sql);
          await client.query('insert into index_migrations (version) values ($1)', [i + 1]);
        }
        await client.query('commit');
      } catch (err) {
        await client.query('rollback').catch(() => {});
        throw err;
      } finally {
        client.release();
      }
    },
    async upsert(session, facts) {
      await pool.query(
        `insert into index_facts (session, id, kind, rev, at, data)
         select $1, * from unnest($2::text[], $3::text[], $4::bigint[], $5::bigint[], $6::jsonb[])
         on conflict (session, id) do update set kind = excluded.kind, rev = excluded.rev, at = excluded.at, data = excluded.data
         where excluded.rev > index_facts.rev`,
        [session, facts.map((f) => f.id), facts.map((f) => f.kind), facts.map((f) => f.rev), facts.map((f) => f.at), facts.map((f) => JSON.stringify(f.data))],
      );
    },
    async facts(kinds) {
      return (await pool.query<FactRow>(
        'select session, id, kind, rev::float8 as rev, at::float8 as at, data from index_facts where $1::text[] is null or kind = any($1)', [kinds ?? null],
      )).rows;
    },
  };
}
