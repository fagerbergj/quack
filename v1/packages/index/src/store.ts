import pg from 'pg';
import type { Fact } from './facts.ts';

// Append-only: a released migration is never edited.
const MIGRATIONS = [
  `create table index_facts (
     session text not null, id text not null, kind text not null, rev bigint not null, at bigint not null, data jsonb not null,
     primary key (session, id)
   );
   create index index_facts_kind on index_facts (kind, at desc)`,
];

export type IndexStore = ReturnType<typeof createStore>;

/** The derived index (A6) in Postgres; it holds nothing a session log cannot regenerate. */
export function createStore(pool: pg.Pool) {
  let ready: Promise<void> | undefined;

  async function migrate() {
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
  }

  // Memoized once it succeeds, retried while the database is down, so the distributor can start before Postgres does.
  const ensure = () => (ready ??= migrate().catch((err) => { ready = undefined; throw err; }));

  async function query<T extends pg.QueryResultRow>(sql: string, params: unknown[] = []): Promise<T[]> {
    await ensure();
    return (await pool.query<T>(sql, params)).rows;
  }

  return {
    ensure,
    /** Idempotent: a fact lands once per (session, id) and only a higher revision replaces it. */
    async upsert(session: string, facts: Fact[]) {
      if (!facts.length) return;
      await query(
        `insert into index_facts (session, id, kind, rev, at, data)
         select $1, * from unnest($2::text[], $3::text[], $4::bigint[], $5::bigint[], $6::jsonb[])
         on conflict (session, id) do update set kind = excluded.kind, rev = excluded.rev, at = excluded.at, data = excluded.data
         where excluded.rev > index_facts.rev`,
        [session, facts.map((f) => f.id), facts.map((f) => f.kind), facts.map((f) => f.rev), facts.map((f) => f.at), facts.map((f) => JSON.stringify(f.data))],
      );
    },
    async sessions() {
      const rows = await query<{ key: string; agent: string | null; version: number | null; status: string | null; cost: number; at: number }>(
        `select session as key,
           max(data->>'agent') filter (where kind = 'session') as agent,
           max((data->>'version')::int) filter (where kind = 'session') as version,
           max(data->>'state') filter (where kind = 'status') as status,
           coalesce(max((data->>'total')::float8) filter (where kind = 'cost'), 0) as cost,
           max(at)::float8 as at
         from index_facts group by session order by max(at) desc, session`,
      );
      return rows.map(({ at, ...s }) => ({ ...s, lastActivity: new Date(at).toISOString() }));
    },
    async artifacts() {
      return query(
        `select session, data->>'kind' as kind, (data->>'version')::int as version, data->>'hash' as hash, data->'verdict' as verdict, at::float8 as "sealedAt"
         from index_facts where kind = 'artifact' order by at desc, session, id`,
      );
    },
    async approvals(state?: string) {
      return query(
        `select session, substr(id, 10) as id, data->>'tool' as tool, data->>'state' as state, data->>'decision' as decision, data->>'by' as by, at::float8 as at
         from index_facts where kind = 'approval' and ($1::text is null or data->>'state' = $1) order by at desc, session, id`,
        [state ?? null],
      );
    },
  };
}
