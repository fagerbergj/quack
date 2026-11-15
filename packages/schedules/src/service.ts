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

const DEFAULT_PAGE_SIZE = 100;
const MAX_PAGE_SIZE = 1000;
// The ScheduleBody fields: id comes from the path, so a body id is unknown.
const FIELDS = new Set(['agent', 'cron', 'tz', 'times', 'sessionKey', 'input', 'deadline']);

class BadRequest extends Error {}
class TooLarge extends Error {}

const http = serviceHttp();
const { send } = http;

const encodeToken = (id: string) => Buffer.from(JSON.stringify(['schedules', id])).toString('base64url');

// Only a token this service issued decodes: it must re-encode to itself.
function decodeToken(token: string | null): string {
  if (!token) return '';
  try {
    const [tag, id] = JSON.parse(Buffer.from(token, 'base64url').toString()) as [unknown, unknown];
    if (tag === 'schedules' && typeof id === 'string' && encodeToken(id) === token) return id;
  } catch { /* falls through */ }
  throw new BadRequest('pageToken is not one this service issued');
}

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

  type Row = { id: string; def: string; created: string };
  const rows = () => db.prepare('SELECT * FROM schedules ORDER BY id').all() as Row[];
  const rowsAfter = (id: string, limit: number) => db.prepare('SELECT * FROM schedules WHERE id > ? ORDER BY id LIMIT ?').all(id, limit) as Row[];
  const rowOf = (id: string) => db.prepare('SELECT * FROM schedules WHERE id = ?').get(id) as Row | undefined;
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
    if (!s.times?.length) return;
    const final = new Date(Math.max(...s.times.map((t) => Date.parse(t)))).toISOString();
    const last = db.prepare(`SELECT MAX(occurrence) AS o FROM runs WHERE schedule = ?`).get(row.id) as { o: string | null };
    if (last.o !== null && last.o >= final) db.prepare(`DELETE FROM schedules WHERE id = ?`).run(row.id);
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

  function view(r: Row) {
    const s = JSON.parse(r.def) as Schedule;
    return { ...s, lastRun: runsOf(r.id, 'fired').at(-1) ?? null, nextRun: nextAfter(s, now())?.toISOString() ?? null, missedRuns: runsOf(r.id, 'missed') };
  }

  // A page of schedules by id; fetching one extra row says whether another page follows.
  function list(url: URL) {
    const size = url.searchParams.get('pageSize');
    const limit = size === null ? DEFAULT_PAGE_SIZE : /^\d+$/.test(size) ? Number(size) : 0;
    if (limit < 1 || limit > MAX_PAGE_SIZE) throw new BadRequest(`pageSize must be an integer from 1 to ${MAX_PAGE_SIZE}`);
    const found = rowsAfter(decodeToken(url.searchParams.get('pageToken')), limit + 1);
    const page = found.slice(0, limit);
    return { schedules: page.map(view), ...(found.length > limit && { nextPageToken: encodeToken(page.at(-1)!.id) }) };
  }

  function register(id: string, raw: unknown): { status: number; body: unknown } {
    if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) throw new BadRequest('body must be a JSON object');
    const unknown = Object.keys(raw).filter((k) => !FIELDS.has(k));
    if (unknown.length) throw new BadRequest(`unknown field: ${unknown.join(', ')}`);
    let s: Schedule;
    try {
      s = validate({ ...(raw as Omit<Schedule, 'id'>), id });
    } catch (err) {
      throw new BadRequest((err as Error).message);
    }
    const existed = rowOf(id) !== undefined;
    upsert.run(id, 'api', JSON.stringify(s), now().toISOString());
    return { status: existed ? 200 : 201, body: view(rowOf(id)!) };
  }

  const methodNotAllowed = (res: ServerResponse, allow: string) => (res.setHeader('allow', allow), sendProblem(res, 405, 'method not allowed'));

  async function handle(req: IncomingMessage, res: ServerResponse) {
    try {
      const url = new URL(req.url ?? '', 'http://x');
      const method = req.method ?? 'GET';
      if (url.pathname === '/') return method === 'GET' ? send(res, 200, list(url)) : methodNotAllowed(res, 'GET');
      const one = /^\/([^/]+)$/.exec(url.pathname);
      if (!one) return sendProblem(res, 404, 'not found');
      let id: string;
      try {
        id = decodeURIComponent(one[1]!);
      } catch {
        throw new BadRequest('scheduleId is not valid percent-encoding');
      }
      if (method === 'PUT') {
        const { status, body: out } = register(id, await body(req));
        return send(res, status, out);
      }
      if (method !== 'DELETE') return methodNotAllowed(res, 'DELETE, PUT');
      if (!db.prepare('DELETE FROM schedules WHERE id = ?').run(id).changes) return sendProblem(res, 404, 'no such schedule');
      res.writeHead(204).end();
    } catch (err) {
      if (err instanceof TooLarge) return sendProblem(res, 413, 'body too large');
      sendProblem(res, err instanceof BadRequest ? 400 : 500, (err as Error).message);
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
