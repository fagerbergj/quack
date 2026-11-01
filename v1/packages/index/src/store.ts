import type { Fact } from './facts.ts';

export type FactRow = Fact & { session: string };

/** One database behind the index: the only part that differs between SQLite and Postgres. */
export type Backend = {
  close(): void;
  migrate(): Promise<void>;
  /** Idempotent: a fact lands once per (session, id) and only a higher revision replaces it. */
  upsert(session: string, facts: Fact[]): Promise<void>;
  facts(kinds?: string[]): Promise<FactRow[]>;
};

export type IndexStore = ReturnType<typeof createStore>;

const newestFirst = (a: { at: number; session: string; id: string }, b: typeof a) => b.at - a.at || a.session.localeCompare(b.session) || a.id.localeCompare(b.id);
const field = (f: FactRow | undefined, key: string) => (f?.data as Record<string, unknown> | undefined)?.[key];

/** The derived index (A6); it holds nothing a session log cannot regenerate. Reads are shaped here so both backends answer alike. */
export function createStore(backend: Backend) {
  let ready: Promise<void> | undefined;
  // Memoized once it succeeds, retried while the database is down, so the distributor can start before Postgres does.
  const ensure = () => (ready ??= backend.migrate().catch((err) => { ready = undefined; throw err; }));
  const facts = async (kinds?: string[]) => { await ensure(); return backend.facts(kinds); };

  return {
    ensure,
    facts,
    async upsert(session: string, batch: Fact[]) {
      if (!batch.length) return;
      await ensure();
      await backend.upsert(session, batch);
    },
    // shortcut: aggregated in memory from every fact row, move the grouping into SQL when the index outgrows one process's memory.
    async sessions() {
      const bySession = Map.groupBy(await facts(), (f) => f.session);
      return [...bySession].map(([key, rows]) => {
        const of = (kind: string) => rows.find((f) => f.kind === kind);
        return {
          key, agent: (field(of('session'), 'agent') as string) ?? null, version: (field(of('session'), 'version') as number) ?? null,
          status: (field(of('status'), 'state') as string) ?? null, cost: (field(of('cost'), 'total') as number) ?? 0, at: Math.max(...rows.map((f) => f.at)),
        };
      }).sort((a, b) => b.at - a.at || a.key.localeCompare(b.key)).map(({ at, ...s }) => ({ ...s, lastActivity: new Date(at).toISOString() }));
    },
    async artifacts() {
      return (await facts(['artifact'])).sort(newestFirst).map((f) => ({
        session: f.session, kind: field(f, 'kind'), version: field(f, 'version'), hash: field(f, 'hash'), verdict: field(f, 'verdict'), sealedAt: f.at,
      }));
    },
    async decisions(state?: string) {
      return (await facts(['decision'])).filter((f) => !state || field(f, 'state') === state).sort(newestFirst).map((f) => ({
        session: f.session, step: field(f, 'step'), question: field(f, 'question'), options: field(f, 'options'), evidence: field(f, 'evidence'),
        state: field(f, 'state'), choice: field(f, 'choice'), by: field(f, 'by'), at: f.at,
      }));
    },
    async approvals(state?: string) {
      return (await facts(['approval'])).filter((f) => !state || field(f, 'state') === state).sort(newestFirst).map((f) => ({
        session: f.session, id: f.id.slice('approval:'.length), tool: field(f, 'tool'), state: field(f, 'state'), decision: field(f, 'decision'), by: field(f, 'by'), at: f.at,
      }));
    },
  };
}
