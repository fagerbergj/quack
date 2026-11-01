import { timingSafeEqual } from 'node:crypto';
import { readdir, readFile } from 'node:fs/promises';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { between, expiry, nextAfter, validate, type Schedule } from './schedule.ts';

export type ServiceConfig = {
  /** shortcut: one shared bearer token authenticates callers until workload tokens (A12) land. */
  serviceToken: string;
  distributorUrl: string;
  /** SQLite file on the service's volume (SCH-2); ':memory:' for tests. */
  dbPath: string;
  /** Same layout the distributor reads: <dir>/<agent>/agent.json, whose optional `schedules` array registers schedules. */
  agentsDir?: string;
  now?: () => Date;
  fetch?: typeof fetch;
  tickMs?: number;
};

const MAX_BODY = 1 << 20;
const send = (res: ServerResponse, status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));

const isMissing = (err: unknown) => (err as NodeJS.ErrnoException).code === 'ENOENT';
const parseJson = (text: string) => {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
};

class BadRequest extends Error {}

function authorized(req: IncomingMessage, token: string): boolean {
  const given = Buffer.from(req.headers.authorization ?? '');
  const expected = Buffer.from(`Bearer ${token}`);
  return given.length === expected.length && timingSafeEqual(given, expected);
}

async function body(req: IncomingMessage): Promise<any> {
  let size = 0;
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    size += chunk.length;
    if (size > MAX_BODY) throw new BadRequest('body too large');
    chunks.push(chunk);
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString('utf8'));
  } catch {
    throw new BadRequest('body is not JSON');
  }
}

export function createSchedulesService(config: ServiceConfig) {
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

  // Agent definitions are re-read each tick, as the distributor does, so a redeploy's schedules take effect without a restart.
  async function syncAgents() {
    if (!config.agentsDir) return;
    // Only a missing dir or file means "no agents"; any other I/O error keeps the stored schedules this tick, since the sync deletes.
    const dirs = await readdir(config.agentsDir, { withFileTypes: true }).catch((err) => (isMissing(err) ? [] : (err as Error)));
    if (dirs instanceof Error) return console.error(`schedules: agents dir: ${dirs.message}`);
    const wanted = new Map<string, Schedule>();
    for (const d of dirs.filter((e) => e.isDirectory())) {
      const text = await readFile(join(config.agentsDir, d.name, 'agent.json'), 'utf8').catch((err) => (isMissing(err) ? '' : (err as Error)));
      if (text instanceof Error) return console.error(`schedules: ${d.name}: ${text.message}`);
      const def = text ? parseJson(text) : null;
      if (typeof def?.name !== 'string' || !Array.isArray(def.schedules)) continue;
      for (const s of def.schedules) {
        try {
          const id = `${def.name}/${s.id}`;
          wanted.set(id, validate({ ...s, id, agent: def.name }));
        } catch (err) {
          console.error(`schedules: ${def.name}: ${(err as Error).message}`);
        }
      }
    }
    for (const [id, s] of wanted) upsert.run(id, 'agent', JSON.stringify(s), now().toISOString());
    for (const r of rows()) if (r.source === 'agent' && !wanted.has(r.id)) db.prepare('DELETE FROM schedules WHERE id = ?').run(r.id);
  }

  async function submit(s: Schedule, occurrence: Date): Promise<boolean> {
    const key = s.sessionKey.replaceAll('{time}', occurrence.toISOString());
    const url = `${config.distributorUrl}/sessions/${encodeURIComponent(key)}/invocations${s.agent ? `?agent=${encodeURIComponent(s.agent)}` : ''}`;
    // The request id makes a retry or duplicate firing one submission (HAR-5).
    const requestId = `schedule:${s.id}:${occurrence.toISOString()}`;
    return (await doFetch(url, { method: 'POST', body: JSON.stringify({ input: s.input, requestId }) })).ok;
  }

  // Handles every occurrence since the last recorded one: the newest useful one fires, the rest are recorded missed (SCH-4).
  // The same path serves live firing and catch-up after downtime.
  async function runDue(row: Row, at: Date) {
    const s = JSON.parse(row.def) as Schedule;
    const last = db.prepare(`SELECT MAX(occurrence) AS o FROM runs WHERE schedule = ?`).get(row.id) as { o: string | null };
    // Explicit times have no backlog to bound, and a registrar's clock runs behind `created`, so a time registered late must still count; a cron starts at `created`.
    const after = last.o ? new Date(last.o) : s.times ? new Date(0) : new Date(row.created);
    const [newest, ...older] = between(s, after, at);
    if (!newest) return;
    const useful = +at < expiry(s, newest);
    if (useful && !(await submit(s, newest))) return;
    record.run(row.id, newest.toISOString(), useful ? 'fired' : 'missed', at.toISOString());
    for (const o of older) record.run(row.id, o.toISOString(), 'missed', at.toISOString());
  }

  async function tick() {
    await syncAgents();
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
    const existing = rows().find((r) => r.id === s.id);
    if (existing && existing.source !== 'api') return { status: 409, body: { error: `${s.id} is declared by an agent` } };
    upsert.run(s.id, 'api', JSON.stringify(s), now().toISOString());
    return { status: 201, body: { id: s.id } };
  }

  const server = createServer(async (req, res) => {
    try {
      if (!authorized(req, config.serviceToken)) return send(res, 401, { error: 'unauthorized' });
      const path = new URL(req.url ?? '', 'http://x').pathname;
      if (req.method === 'GET' && path === '/schedules') return send(res, 200, { schedules: list() });
      if (req.method === 'POST' && path === '/schedules') {
        const { status, body: out } = register(await body(req));
        return send(res, status, out);
      }
      const one = /^\/schedules\/(.+)$/.exec(path);
      if (req.method === 'DELETE' && one) {
        const id = decodeURIComponent(one[1]);
        const removed = db.prepare(`DELETE FROM schedules WHERE id = ? AND source = 'api'`).run(id).changes;
        return send(res, removed ? 200 : 404, removed ? { id } : { error: 'no such api schedule' });
      }
      send(res, 404, { error: 'not found' });
    } catch (err) {
      send(res, err instanceof BadRequest ? 400 : 500, { error: (err as Error).message });
    }
  });

  let timer: NodeJS.Timeout | undefined;
  let stopped = false;
  // A failed tick (a busy database, a disk error) is logged and retried: an unhandled rejection would kill the process.
  const loop = (): Promise<void> =>
    tick()
      .catch((err) => console.error(`schedules: tick failed: ${err}`))
      .finally(() => {
        if (!stopped) timer = setTimeout(loop, config.tickMs ?? 10_000);
      });
  return Object.assign(server, {
    tick,
    /** Starts the timer loop and resolves once the first tick, the downtime catch-up, has settled. */
    start: loop,
    stop() {
      stopped = true;
      clearTimeout(timer);
      server.close();
      db.close();
    },
  });
}
