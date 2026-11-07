import { compileWhere, ftsText, type Ast, type Param, type QueryDialect, type Table } from './compile.ts';
import { keyset } from './keyset.ts';
import type {
  ActionWrite, ArtifactRow, AskRow, EventWrite, Json, ListOptions, ListPage, Principal, RunOutcome, SessionCatalogStore, SessionRow, SessionState, Write,
} from './port.ts';

type Row = Record<string, unknown>;
export type Sql = { all(sql: string, params?: Param[]): Promise<Row[]>; run(sql: string, params?: Param[]): Promise<void> };

/** One database behind the catalog: the only part that differs between SQLite and PostgreSQL. */
export type Driver = QueryDialect & {
  /** One transaction that holds the session's write lock for its whole length. */
  tx<T>(key: string, fn: (sql: Sql) => Promise<T>): Promise<T>;
  read<T>(fn: (sql: Sql) => Promise<T>): Promise<T>;
  migrate(): Promise<void>;
  close(): Promise<void>;
};

const SESSION_COLS = 's.key, s.incarnation, s.agent, s.title, s.default_title, s.custom_title, s.state, s.created_at, s.last_activity, s.currency, cast(s.cost_micros as text) as cost_text, '
  + 's.tokens_input, s.tokens_output, s.tokens_cache_read, s.tokens_cache_write, s.pending_asks, s.oldest_ask_at, s.last_outcome, s.last_ended_at, s.failed_steps, s.last_message, s.watermark';
const ASK_COLS = 'a.session, a.id, a.type, a.state, a.created_at, a.resolved_at, a.resolution, a.tool, a.args, a.prompt';
const ARTIFACT_COLS = 'r.session, r.kind, r.version, r.hash, r.content, r.sealed_at, r.summary, r.deliveries';

const num = (v: unknown) => Number(v);
const opt = (v: unknown) => (v === null || v === undefined ? undefined : v);
const json = (v: unknown) => (v === null || v === undefined ? undefined : JSON.parse(String(v)));

function sessionRow(r: Row): SessionRow {
  return {
    key: String(r.key), agent: String(r.agent), ...(opt(r.title) !== undefined && { title: String(r.title) }), state: r.state as SessionState,
    createdAt: num(r.created_at), lastActivity: num(r.last_activity), cost: { currencyCode: String(r.currency), amountMicros: BigInt(String(r.cost_text)) },
    tokens: { input: num(r.tokens_input), output: num(r.tokens_output), cacheRead: num(r.tokens_cache_read), cacheWrite: num(r.tokens_cache_write) },
    pendingAsks: num(r.pending_asks), ...(opt(r.oldest_ask_at) !== undefined && { oldestAskAt: num(r.oldest_ask_at) }),
    ...(opt(r.last_outcome) !== undefined && { lastRun: { outcome: r.last_outcome as RunOutcome, endedAt: num(r.last_ended_at), failedSteps: json(r.failed_steps) ?? [] } }),
    ...(opt(r.last_message) !== undefined && { lastMessage: String(r.last_message) }),
  };
}

function askRow(r: Row): AskRow {
  return {
    session: String(r.session), id: String(r.id), type: r.type as AskRow['type'], state: r.state as AskRow['state'], createdAt: num(r.created_at),
    ...(opt(r.resolved_at) !== undefined && { resolvedAt: num(r.resolved_at) }), ...(opt(r.resolution) !== undefined && { resolution: json(r.resolution) }),
    ...(opt(r.tool) !== undefined && { toolCall: { tool: String(r.tool), args: json(r.args) ?? null } }), ...(opt(r.prompt) !== undefined && { prompt: json(r.prompt) }),
  };
}

// The one place a stored artifact becomes the port's row.
function artifactRow(r: Row): ArtifactRow {
  const deliveries = json(r.deliveries) as ArtifactRow['deliveries'];
  return {
    session: String(r.session), kind: String(r.kind), version: num(r.version), hash: String(r.hash), content: String(r.content), sealedAt: num(r.sealed_at),
    ...(opt(r.summary) !== undefined && { summary: String(r.summary) }), ...(deliveries?.length && { deliveries }),
  };
}

// The lifecycle table: target state, or what the move is when it does not apply.
function nextState(from: SessionState, to: SessionState): SessionState | 'noop' | 'refused' {
  if (from === to) return 'noop';
  // idle is a run ending or a process coming up without one; only invocation or unarchive leaves archived.
  if (to === 'idle' || to === 'stopped') return from === 'archived' ? 'refused' : to;
  if (to === 'archived') return from === 'running' ? 'refused' : 'archived';
  return 'running';
}

/** The catalog store over one SQL driver: every write is one transaction holding the session's lock. */
export function sqlStore(driver: Driver, opts: { currency: string }): SessionCatalogStore {
  const load = async (sql: Sql, key: string) => (await sql.all(`select ${SESSION_COLS} from catalog_sessions s where s.key = ?`, [key]))[0];
  const loadAsk = async (sql: Sql, key: string, id: string) => (await sql.all(`select ${ASK_COLS} from catalog_asks a where a.session = ? and a.id = ?`, [key, id]))[0];
  const loadArtifact = async (sql: Sql, key: string, kind: string, version: number) =>
    (await sql.all(`select ${ARTIFACT_COLS} from catalog_artifacts r where r.session = ? and r.kind = ? and r.version = ?`, [key, kind, version]))[0];

  /** Applies an event above the watermark and moves the watermark to it, unless `holds` says the write was refused. */
  function onEvent<T>(key: string, w: EventWrite, skip: T, apply: (sql: Sql, row: Row) => Promise<T>, holds: (out: T) => boolean = () => false): Promise<T> {
    return driver.tx(key, async (sql) => {
      const row = await load(sql, key);
      if (!row || w.seq <= num(row.watermark)) return skip;
      const out = await apply(sql, row);
      if (!holds(out)) await sql.run('update catalog_sessions set watermark = ? where key = ?', [w.seq, key]);
      return out;
    });
  }

  /** Applies an action once per id; a missing (absent or tombstoned) session refuses it. */
  function onAction<T>(key: string, w: ActionWrite, refused: T, apply: (sql: Sql, row: Row, repeat: boolean) => Promise<T>): Promise<T> {
    return driver.tx(key, async (sql) => {
      const row = await load(sql, key);
      if (!row) return refused;
      const repeat = (await sql.all('select 1 as x from catalog_actions where session = ? and id = ?', [key, w.id])).length > 0;
      const out = await apply(sql, row, repeat);
      if (!repeat) await sql.run('insert into catalog_actions (session, id) values (?, ?)', [key, w.id]);
      return out;
    });
  }

  const onWrite = <T>(key: string, w: Write, skip: T, apply: (sql: Sql, row: Row, repeat: boolean) => Promise<T>) =>
    w.kind === 'event' ? onEvent(key, w, skip, (sql, row) => apply(sql, row, false)) : onAction(key, w, skip, apply);

  const setState = (sql: Sql, key: string, state: SessionState) => sql.run('update catalog_sessions set state = ? where key = ?', [state, key]);
  const refreshText = (sql: Sql, key: string) => sql.all('select title, last_message from catalog_sessions where key = ?', [key])
    .then(([r]) => sql.run('update catalog_sessions set fts_text = ? where key = ?', [ftsText(opt(r!.title) as string, key, opt(r!.last_message) as string), key]));
  const recount = (sql: Sql, key: string) => sql.run(
    `update catalog_sessions set pending_asks = (select count(*) from catalog_asks a where a.session = ? and a.state = 'pending'),
       oldest_ask_at = (select min(a.created_at) from catalog_asks a where a.session = ? and a.state = 'pending') where key = ?`, [key, key, key]);

  async function move(sql: Sql, row: Row, to: SessionState, repeat: boolean, rule = nextState) {
    const from = row.state as SessionState;
    const next = repeat ? 'noop' : rule(from, to);
    if (next === 'noop' || next === 'refused') return { applied: false, state: from };
    await setState(sql, String(row.key), next);
    return { applied: true, state: next };
  }

  async function removeRows(sql: Sql, key: string) {
    for (const table of ['catalog_asks', 'catalog_artifacts', 'catalog_actions', 'catalog_deliveries']) await sql.run(`delete from ${table} where session = ?`, [key]);
    await sql.run('delete from catalog_sessions where key = ?', [key]);
  }

  async function list<T>(table: Table, alias: string, cols: string, options: ListOptions<string, { field: string; direction: 'newest' | 'oldest' }>, map: (r: Row) => T): Promise<ListPage<T>> {
    const ks = keyset(table, alias, options.where, options.orderBy);
    const where = compileWhere(driver, table, alias, options.where as Ast);
    const after = ks.after(options.cursor);
    const rows = await driver.read((sql) => sql.all(
      `select ${cols}, ${ks.select} from catalog_${table} ${alias} where ${where.sql} and ${after.sql} order by ${ks.orderBy} limit ?`,
      [...where.params, ...after.params, options.pageSize + 1]));
    const page = rows.slice(0, options.pageSize);
    return { rows: page.map(map), ...(rows.length > options.pageSize && { cursor: ks.next(page.at(-1)!) }) };
  }

  return {
    migrate: () => driver.migrate(),
    close: () => driver.close(),

    createSession(w, { key, agent, createdAt }) {
      return driver.tx(key, async (sql) => {
        const row = await load(sql, key);
        const [tomb] = await sql.all('select incarnation from catalog_tombstones where session = ?', [key]);
        if (tomb?.incarnation === w.id || (row && (w.seq <= num(row.watermark) || row.incarnation === w.id))) {
          if (row && w.seq > num(row.watermark)) await sql.run('update catalog_sessions set agent = ?, created_at = ?, watermark = ? where key = ?', [agent, createdAt, w.seq, key]);
          return;
        }
        // Another incarnation's create: a reused key starts fresh.
        await removeRows(sql, key);
        await sql.run('delete from catalog_tombstones where session = ?', [key]);
        await sql.run(
          `insert into catalog_sessions (key, incarnation, agent, state, created_at, last_activity, currency, cost_micros, tokens_input, tokens_output, tokens_cache_read,
             tokens_cache_write, pending_asks, watermark, fts_text) values (?, ?, ?, 'idle', ?, ?, ?, 0, 0, 0, 0, 0, 0, ?, ?)`,
          [key, w.id, agent, createdAt, createdAt, opts.currency, w.seq, ftsText(key)]);
      });
    },

    async watermark(key) {
      const [row] = await driver.read((sql) => sql.all('select watermark from catalog_sessions where key = ?', [key]));
      return row ? num(row.watermark) : undefined;
    },

    transitionSession(w: Write, { key, to }: { key: string; to: SessionState }) {
      return onWrite(key, w, { applied: false } as { applied: boolean; state?: SessionState }, (sql, row, repeat) => move(sql, row, to, repeat));
    },

    setTitle(w, { key, title }) {
      return onWrite(key, w, undefined, async (sql, row, repeat) => {
        if (repeat) return;
        const [def, custom] = w.kind === 'event' ? [title, opt(row.custom_title) ?? null] : [opt(row.default_title) ?? null, title];
        await sql.run('update catalog_sessions set default_title = ?, custom_title = ?, title = ? where key = ?', [def as string | null, custom as string | null, (custom ?? def) as string | null, key]);
        await refreshText(sql, key);
      });
    },

    archiveSession(w, { key }) {
      return onAction<{ applied: boolean; state?: SessionState }>(key, w, { applied: false }, (sql, row, repeat) => move(sql, row, 'archived', repeat));
    },

    unarchiveSession(w, { key }) {
      return onAction<{ applied: boolean; state?: SessionState }>(key, w, { applied: false }, (sql, row, repeat) => move(sql, row, 'stopped', repeat, (from) => (from === 'archived' ? 'stopped' : 'noop')));
    },

    recordRunOutcome(w, o) {
      return onEvent(o.key, w, undefined, async (sql, row) => {
        await sql.run('update catalog_sessions set last_outcome = ?, last_ended_at = ?, failed_steps = ?, last_activity = ?, last_message = ? where key = ?',
          [o.outcome, o.endedAt, JSON.stringify(o.failedSteps), o.endedAt, (o.lastMessage ?? opt(row.last_message) ?? null) as string | null, o.key]);
        await refreshText(sql, o.key);
      });
    },

    recordSpend(w, { key, cost, tokens }) {
      type Out = { applied: boolean; refused?: 'currency' | 'regressed' };
      return onEvent<Out>(key, w, { applied: false }, async (sql, row) => {
        if (cost.currencyCode !== row.currency) return { applied: false, refused: 'currency' };
        if (cost.amountMicros < BigInt(String(row.cost_text))) return { applied: false, refused: 'regressed' };
        await sql.run('update catalog_sessions set cost_micros = ?, tokens_input = ?, tokens_output = ?, tokens_cache_read = ?, tokens_cache_write = ? where key = ?',
          [cost.amountMicros, tokens.input, tokens.output, tokens.cacheRead, tokens.cacheWrite, key]);
        return { applied: true };
      }, (out) => out.refused !== undefined);
    },

    deleteSession(w, { key }) {
      return driver.tx(key, async (sql) => {
        const row = await load(sql, key);
        if (!row) return false;
        await removeRows(sql, key);
        await sql.run('insert into catalog_tombstones (session, incarnation) values (?, ?) on conflict (session) do update set incarnation = excluded.incarnation', [key, String(row.incarnation)]);
        return true;
      });
    },

    resetSession({ key }) {
      return driver.tx(key, async (sql) => {
        await removeRows(sql, key);
        await sql.run('delete from catalog_tombstones where session = ?', [key]);
      });
    },

    async getSession(key) {
      const row = await driver.read((sql) => load(sql, key));
      return row && sessionRow(row);
    },

    listSessions: (options) => list('sessions', 's', SESSION_COLS, options, sessionRow),

    openAsk(w, ask) {
      return onEvent(ask.key, w, undefined, async (sql, row) => {
        const old = await loadAsk(sql, ask.key, ask.id);
        if (old && old.state !== 'pending') return;
        const values = [ask.type, ask.createdAt, ask.toolCall?.tool ?? null, ask.toolCall ? JSON.stringify(ask.toolCall.args) : null, ask.prompt ? JSON.stringify(ask.prompt) : null, ftsText(ask.prompt?.question, ask.toolCall?.tool)];
        await sql.run(`insert into catalog_asks (session, id, state, type, created_at, tool, args, prompt, fts_text) values (?, ?, 'pending', ?, ?, ?, ?, ?, ?)
          on conflict (session, id) do update set type = excluded.type, created_at = excluded.created_at, tool = excluded.tool, args = excluded.args, prompt = excluded.prompt, fts_text = excluded.fts_text`,
        [ask.key, ask.id, ...values]);
        await recount(sql, ask.key);
        // Waiting on a person is running.
        if (row.state !== 'running') await setState(sql, ask.key, 'running');
      });
    },

    resolveAsk(w, c) {
      return onWrite(c.key, w, { applied: false } as { applied: boolean; ask?: AskRow }, async (sql, _row, repeat) => {
        const ask = await loadAsk(sql, c.key, c.id);
        if (!ask || repeat || ask.state !== 'pending') return { applied: false, ...(ask && { ask: askRow(ask) }) };
        const resolution: { by: Principal; choice: string; text?: string; reason?: string } = { by: c.by, choice: c.choice, ...(c.text !== undefined && { text: c.text }), ...(c.reason !== undefined && { reason: c.reason }) };
        await sql.run("update catalog_asks set state = 'resolved', resolved_at = ?, resolution = ? where session = ? and id = ?", [c.resolvedAt, JSON.stringify(resolution), c.key, c.id]);
        await recount(sql, c.key);
        return { applied: true, ask: askRow((await loadAsk(sql, c.key, c.id))!) };
      });
    },

    cancelAsk(w, { key, id }) {
      return onEvent(key, w, undefined, async (sql) => {
        await sql.run("update catalog_asks set state = 'cancelled' where session = ? and id = ? and state = 'pending'", [key, id]);
        await recount(sql, key);
      });
    },

    async getAsk(key, id) {
      const row = await driver.read((sql) => loadAsk(sql, key, id));
      return row && askRow(row);
    },

    listAsks: (options) => list('asks', 'a', ASK_COLS, options, askRow),

    sealArtifact(w, a) {
      return onEvent(a.key, w, undefined, async (sql) => {
        await sql.run(`insert into catalog_artifacts (session, kind, version, hash, content, sealed_at, summary, deliveries, fts_text) values (?, ?, ?, ?, ?, ?, ?, '[]', ?)
          on conflict (session, kind, version) do update set hash = excluded.hash, content = excluded.content, sealed_at = excluded.sealed_at, summary = excluded.summary, fts_text = excluded.fts_text`,
        [a.key, a.kind, a.version, a.hash, a.content, a.sealedAt, a.summary ?? null, ftsText(a.summary)]);
      });
    },

    advanceWatermark(w, { key }) {
      return onEvent(key, w, undefined, async () => {});
    },

    recordArtifactDelivery(w, d) {
      return onEvent(d.key, w, undefined, async (sql) => {
        const old = await loadArtifact(sql, d.key, d.kind, d.version);
        if (!old) return;
        // Once per event id, whatever its seq: a superseding revision of the same delivery never appends again.
        const seen = await sql.all('select 1 as x from catalog_deliveries where session = ? and event_id = ?', [d.key, w.id]);
        if (seen.length) return;
        await sql.run('insert into catalog_deliveries (session, event_id) values (?, ?)', [d.key, w.id]);
        const deliveries = [...((json(old.deliveries) as unknown[]) ?? []), { where: d.where, link: d.link, at: d.at }];
        await sql.run('update catalog_artifacts set deliveries = ? where session = ? and kind = ? and version = ?', [JSON.stringify(deliveries), d.key, d.kind, d.version]);
      });
    },

    listArtifacts: (options) => list('artifacts', 'r', ARTIFACT_COLS, options, artifactRow),
  } as SessionCatalogStore;
}

/** The schema both adapters create, with each dialect's column types substituted. */
export function tablesDdl(types: { text: string; int: string }): string {
  const { text: t, int: i } = types;
  return `
    create table catalog_sessions (
      key ${t} primary key, incarnation ${t} not null, agent ${t} not null, title ${t}, default_title ${t}, custom_title ${t}, state ${t} not null,
      created_at ${i} not null, last_activity ${i} not null, currency ${t} not null, cost_micros ${i} not null, tokens_input ${i} not null, tokens_output ${i} not null,
      tokens_cache_read ${i} not null, tokens_cache_write ${i} not null, pending_asks ${i} not null, oldest_ask_at ${i}, last_outcome ${t}, last_ended_at ${i},
      failed_steps ${t}, last_message ${t}, watermark ${i} not null, fts_text ${t} not null
    );
    create index catalog_sessions_activity on catalog_sessions (last_activity desc, key);
    create index catalog_sessions_created on catalog_sessions (created_at desc, key);
    create table catalog_asks (
      session ${t} not null, id ${t} not null, type ${t} not null, state ${t} not null, created_at ${i} not null, resolved_at ${i}, resolution ${t},
      tool ${t}, args ${t}, prompt ${t}, fts_text ${t} not null, primary key (session, id)
    );
    create index catalog_asks_state on catalog_asks (state, created_at, session, id);
    create table catalog_artifacts (
      session ${t} not null, kind ${t} not null, version ${i} not null, hash ${t} not null, content ${t} not null, sealed_at ${i} not null, summary ${t},
      deliveries ${t} not null, fts_text ${t} not null, primary key (session, kind, version)
    );
    create index catalog_artifacts_sealed on catalog_artifacts (sealed_at desc, session, kind, version);
    create table catalog_deliveries (session ${t} not null, event_id ${t} not null, primary key (session, event_id));
    create table catalog_actions (session ${t} not null, id ${t} not null, primary key (session, id));
    create table catalog_tombstones (session ${t} primary key, incarnation ${t} not null)`;
}

export const FTS_TABLES = ['sessions', 'asks', 'artifacts'] as const;
