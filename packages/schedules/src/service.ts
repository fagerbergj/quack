import type { IncomingMessage, ServerResponse } from 'node:http';
import { DatabaseSync } from 'node:sqlite';
import { sendProblem, serviceHttp, submitToSession, tooLarge, type App } from '@quack/sdk';
import { between, expiry, nextAfter, validate, type Schedule } from './schedule.ts';

export type ServiceConfig = {
  distributorUrl: string;
  /** SQLite file on the service's volume; ':memory:' for tests. */
  dbPath: string;
  /** The distributor's QUACK_ADMIN_TOKEN, which a retention sweep needs to delete. */
  distributorToken?: string;
  /** Runs the retention sweep daily; off by default, so deleting is opt-in. */
  retentionSweep?: boolean;
  now?: () => Date;
  fetch?: typeof fetch;
  tickMs?: number;
};

const DAY_MS = 86_400_000;

class BadRequest extends Error {}
class TooLarge extends Error {}

const http = serviceHttp();
const { send } = http;

async function body(req: IncomingMessage): Promise<any> {
  const raw = await http.readBody(req).catch((err) => { throw tooLarge(err) ? new TooLarge() : new BadRequest((err as Error).message); });
  try {
    return JSON.parse(raw.toString('utf8'));
  } catch {
    throw new BadRequest('body is not JSON');
  }
}

export function createSchedulesService(config: ServiceConfig): App & { tick(): Promise<void>; stop(): void } {
  const now = config.now ?? (() => new Date());
  const doFetch = config.fetch ?? fetch;
  const db = new DatabaseSync(config.dbPath);
  db.exec(`
    CREATE TABLE IF NOT EXISTS schedules (id TEXT PRIMARY KEY, source TEXT NOT NULL, def TEXT NOT NULL, created TEXT NOT NULL);
    CREATE TABLE IF NOT EXISTS runs (schedule TEXT NOT NULL, occurrence TEXT NOT NULL, status TEXT NOT NULL, at TEXT NOT NULL, PRIMARY KEY (schedule, occurrence));
  `);
  const upsert = db.prepare(`INSERT INTO schedules (id, source, def, created) VALUES (?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET def = excluded.def, source = excluded.source`);
  const record = db.prepare(`INSERT OR IGNORE INTO runs (schedule, occurrence, status, at) VALUES (?, ?, ?, ?)`);

  type Row = { id: string; source: string; def: string; created: string };
  const rows = () => db.prepare('SELECT * FROM schedules ORDER BY id').all() as Row[];
  const runsOf = (id: string, status: string) => (db.prepare('SELECT occurrence FROM runs WHERE schedule = ? AND status = ? ORDER BY occurrence').all(id, status) as Array<{ occurrence: string }>).map((r) => r.occurrence);

  async function submit(s: Schedule, occurrence: Date): Promise<boolean> {
    // The request id makes a retry or duplicate firing one submission.
    const requestId = `schedule:${s.id}:${occurrence.toISOString()}`;
    return (await submitToSession(config.distributorUrl, { key: s.sessionKey, agent: s.agent, input: s.input, requestId }, doFetch)).ok;
  }

  // Handles every occurrence since the last recorded one: the newest useful one fires, the rest are recorded missed.
  // The same path serves live firing and catch-up after downtime.
  async function runDue(row: Row, at: Date) {
    const s = JSON.parse(row.def) as Schedule;
    const last = db.prepare(`SELECT MAX(occurrence) AS o FROM runs WHERE schedule = ?`).get(row.id) as { o: string | null };
    // Explicit times have no backlog to bound, and a registrar's clock runs behind `created`, so a time registered late must still count; a cron starts at `created`.
    const after = last.o ? new Date(last.o) : s.times ? new Date(0) : new Date(row.created);
    const [newest, ...older] = between(s, after, at);
    if (newest) {
      const useful = +at < expiry(s, newest);
      if (useful && !(await submit(s, newest))) return;
      record.run(row.id, newest.toISOString(), useful ? 'fired' : 'missed', at.toISOString());
      for (const o of older) record.run(row.id, o.toISOString(), 'missed', at.toISOString());
    }
    dropIfFinished(row, s);
  }

  // Each tick reads every row, so a registered one-shot whose last time is recorded goes; its runs stay, so registering that time again never refires it.
  function dropIfFinished(row: Row, s: Schedule) {
    if (row.source !== 'api' || !s.times?.length) return;
    const final = new Date(Math.max(...s.times.map((t) => Date.parse(t)))).toISOString();
    const last = db.prepare(`SELECT MAX(occurrence) AS o FROM runs WHERE schedule = ?`).get(row.id) as { o: string | null };
    if (last.o !== null && last.o >= final) db.prepare(`DELETE FROM schedules WHERE id = ? AND source = 'api'`).run(row.id);
  }

  // The retention each agent's current definition declares, read once per agent per sweep; undefined keeps its sessions.
  async function retentionOf(agents: string[]): Promise<Map<string, number | undefined>> {
    const found = await Promise.all(agents.map(async (name) => {
      const r = await doFetch(`${config.distributorUrl}/agents/${encodeURIComponent(name)}`);
      const view = r.ok ? ((await r.json()) as { definition?: { retention?: { idleDays?: unknown } } }) : undefined;
      const days = view?.definition?.retention?.idleDays;
      return [name, typeof days === 'number' ? days : undefined] as const;
    }));
    return new Map(found);
  }

  // Retention: delete sessions idle longer than their agent declares; the distributor refuses any that are not safe to remove.
  async function sweepRetention() {
    const sessions: Array<{ key: string; agent: string; lastActivity: string }> = [];
    for (let token = ''; ;) {
      const query = new URLSearchParams({ pageSize: '1000', q: '-state:running AND pendingAsks:0', ...(token && { pageToken: token }) });
      const page = (await (await doFetch(`${config.distributorUrl}/sessions?${query}`)).json()) as { sessions: typeof sessions; nextPageToken?: string };
      sessions.push(...page.sessions);
      if (!page.nextPageToken) break;
      token = page.nextPageToken;
    }
    const days = await retentionOf([...new Set(sessions.map((s) => s.agent))]);
    const idleDays = (s: (typeof sessions)[number]) => days.get(s.agent);
    // home:main is the standing Home conversation and must outlive any idle stretch.
    const due = sessions.filter((s) => s.key !== 'home:main' && idleDays(s) !== undefined && +now() - Date.parse(s.lastActivity) > idleDays(s)! * DAY_MS).map((s) => s.key);
    const deleted: string[] = [];
    const refused: Array<{ key: string; status: number }> = [];
    for (const key of due) {
      const r = await doFetch(`${config.distributorUrl}/sessions/${encodeURIComponent(key)}`, { method: 'DELETE', headers: { authorization: `Bearer ${config.distributorToken}` } });
      if (r.ok) deleted.push(key);
      else refused.push({ key, status: r.status });
    }
    return { due, deleted, refused };
  }

  let lastSweep = 0;
  async function tick() {
    if (config.retentionSweep && +now() - lastSweep >= DAY_MS) {
      // Recorded only on success: a deploy restarts us with a still-booting distributor, and the next tick should retry.
      const at = +now();
      await sweepRetention().then(() => (lastSweep = at), (err) => console.error(`schedules: retention: ${err}`));
    }
    const at = now();
    for (const row of rows()) {
      try {
        await runDue(row, at);
      } catch (err) {
        console.error(`schedules: ${row.id}: ${(err as Error).message}`);
      }
    }
  }

  function list() {
    const at = now();
    return rows().map((r) => {
      const s = JSON.parse(r.def) as Schedule;
      return { ...s, source: r.source, lastRun: runsOf(r.id, 'fired').at(-1) ?? null, nextRun: nextAfter(s, at)?.toISOString() ?? null, missedRuns: runsOf(r.id, 'missed') };
    });
  }

  function register(raw: any): { status: number; body: unknown } {
    let s: Schedule;
    try {
      s = validate(raw);
    } catch (err) {
      throw new BadRequest((err as Error).message);
    }
    upsert.run(s.id, 'api', JSON.stringify(s), now().toISOString());
    return { status: 201, body: { id: s.id } };
  }

  function removeApi(id: string, res: ServerResponse) {
    const removed = db.prepare(`DELETE FROM schedules WHERE id = ? AND source = 'api'`).run(id).changes;
    send(res, removed ? 200 : 404, removed ? { id } : { error: 'no such api schedule' });
  }

  async function handle(req: IncomingMessage, res: ServerResponse) {
    try {
      const path = new URL(req.url ?? '', 'http://x').pathname;
      if (req.method === 'GET' && path === '/') return send(res, 200, { schedules: list() });
      if (req.method === 'POST' && path === '/') {
        const { status, body: out } = register(await body(req));
        return send(res, status, out);
      }
      const one = /^\/(.+)$/.exec(path);
      if (req.method === 'DELETE' && one) return removeApi(decodeURIComponent(one[1]), res);
      send(res, 404, { error: 'not found' });
    } catch (err) {
      if (err instanceof TooLarge) return sendProblem(res, 413, 'body too large');
      send(res, err instanceof BadRequest ? 400 : 500, { error: (err as Error).message });
    }
  }

  let timer: NodeJS.Timeout | undefined;
  let stopped = false;
  // A failed tick (a busy database, a disk error) is logged and retried: an unhandled rejection would kill the process.
  const loop = (): Promise<void> =>
    tick()
      .catch((err) => console.error(`schedules: tick failed: ${err}`))
      .finally(() => {
        if (!stopped) timer = setTimeout(loop, config.tickMs ?? 10_000);
      });
  return {
    handle,
    tick,
    /** Starts the timer loop and resolves once the first tick, the downtime catch-up, has settled. */
    start: loop,
    stop() {
      stopped = true;
      clearTimeout(timer);
      db.close();
    },
  };
}
