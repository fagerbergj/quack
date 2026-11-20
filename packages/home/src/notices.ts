import { DatabaseSync } from 'node:sqlite';
import { escapeTerm, isSessionKey, sendProblem, serviceHttp, submitToSession, type App, type AppContext, type RunEnded } from '@quack/sdk';
import { sessionLink } from './tools.ts';

/** The standing Home conversation, which hears how the sessions it started ended. */
export const HOME = { key: 'home:main', agent: 'quack' };
const MAX_SUMMARY = 600;
const http = serviceHttp({ maxBody: 4096 });

const oneLine = (s: string, n: number) => {
  const t = s.replace(/\s+/g, ' ').trim();
  return t.length > n ? `${t.slice(0, n)}…` : t;
};

type Notice = { id: string; session: string; outcome: RunEnded['data']['outcome']; failed: string | null };

// A sent notice only outlives its event's redelivery after a restart; a key idle this long is a session nobody awaits.
const SENT_KEEP_MS = 30 * 86_400_000;
const STARTED_KEEP_MS = 90 * 86_400_000;

/** Home's app: it records the keys Home's start action returned, and tells home:main, as a follow-up, how each of their runs ended. */
export function homeApp(ctx: Pick<AppContext, 'dbPath' | 'distributorUrl' | 'fetch' | 'subscribe'>, now: () => number = Date.now): App {
  const db = new DatabaseSync(ctx.dbPath);
  db.exec(`CREATE TABLE IF NOT EXISTS started (key TEXT PRIMARY KEY, at INTEGER NOT NULL DEFAULT 0);
    CREATE TABLE IF NOT EXISTS notices (id TEXT PRIMARY KEY, session TEXT NOT NULL, outcome TEXT NOT NULL, failed TEXT, sent INTEGER NOT NULL DEFAULT 0, at INTEGER NOT NULL DEFAULT 0)`);
  // Tables from before pruning have no `at`; their rows start their retention now.
  for (const table of ['started', 'notices']) {
    if (!(db.prepare(`PRAGMA table_info(${table})`).all() as Array<{ name: string }>).some((c) => c.name === 'at')) db.exec(`ALTER TABLE ${table} ADD COLUMN at INTEGER NOT NULL DEFAULT 0`);
    db.prepare(`UPDATE ${table} SET at = ? WHERE at = 0`).run(now());
  }
  // Unsent notices are never pruned: they are what a restart must still deliver.
  const prune = () => {
    db.prepare('DELETE FROM notices WHERE sent = 1 AND at < ?').run(now() - SENT_KEEP_MS);
    db.prepare('DELETE FROM started WHERE at < ?').run(now() - STARTED_KEEP_MS);
  };
  const touch = db.prepare('INSERT INTO started (key, at) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET at = excluded.at');
  const isStarted = db.prepare('SELECT 1 FROM started WHERE key = ?');
  // Recorded synchronously, before the channel acks the batch, so a crash can repeat a notice's send but never lose it.
  const record = db.prepare('INSERT OR IGNORE INTO notices (id, session, outcome, failed, at) VALUES (?, ?, ?, ?, ?)');

  const get = async <T>(path: string): Promise<T | undefined> => {
    const res = await ctx.fetch(`${ctx.distributorUrl}${path}`);
    return res.ok ? ((await res.json()) as T) : undefined;
  };

  async function text(n: Notice): Promise<string> {
    const q = new URLSearchParams({ q: `key:${escapeTerm(n.session)}` });
    const [row] = (await get<{ sessions: Array<{ title?: string }> }>(`/sessions?${q}`))?.sessions ?? [];
    const [result] = (await get<{ artifacts: Array<{ kind: string; version: number; summary?: string }> }>(`/sessions/${encodeURIComponent(n.session)}/artifacts?pageSize=1`))?.artifacts ?? [];
    const lines = [`A session you started ended: ${row?.title || n.session} (key ${n.session}), ${n.outcome}.`];
    if (n.failed) lines.push(`Failed at ${oneLine(n.failed, 300)}`);
    lines.push(result ? `Newest result: ${result.kind} v${result.version}${result.summary ? `: ${oneLine(result.summary, MAX_SUMMARY)}` : ''}` : 'No sealed result.');
    lines.push(`Link ${sessionLink(n.session)}`);
    return lines.join('\n');
  }

  // One pass at a time; a call while draining marks it dirty so a notice recorded as a pass ends is not stranded.
  let draining: Promise<void> | undefined;
  let dirty = false;
  const unsent = db.prepare('SELECT id, session, outcome, failed FROM notices WHERE sent = 0 ORDER BY rowid LIMIT 1');
  async function drainOnce() {
    for (let n = unsent.get() as Notice | undefined; n; n = unsent.get() as Notice | undefined) {
      const res = await submitToSession(ctx.distributorUrl, { ...HOME, input: await text(n), requestId: `home-notice:${n.id}`, whenBusy: 'followUp' }, ctx.fetch);
      if (!res.ok) return console.error(`home: notice for ${n.session} not delivered: HTTP ${res.status}`);
      db.prepare('UPDATE notices SET sent = 1 WHERE id = ?').run(n.id);
    }
  }
  async function drainLoop() {
    try {
      do { dirty = false; await drainOnce().catch((err) => console.error('home: notices:', err)); } while (dirty);
    } finally {
      draining = undefined;
    }
  }
  function drain() {
    if (draining) { dirty = true; return draining; }
    return (draining = drainLoop());
  }

  // shortcut: a run that ends before the start tool records its key goes unheard; session boot makes that window small, so reconcile with the catalog on record if it shows up.
  const unsubscribe = ctx.subscribe((session, event) => {
    if (event.type !== 'run.ended' || !isStarted.get(session)) return;
    const { outcome, failedSteps } = (event as RunEnded).data;
    const first = failedSteps[0];
    // The event id is per session, so the notice's key carries both.
    record.run(`${session}\n${event.id}`, session, outcome, first ? `${first.step}: ${first.error}` : null, now());
    touch.run(session, now());
    prune();
    void drain();
  });

  return {
    async handle(req, res) {
      if (req.method !== 'POST' || req.url !== '/started') return sendProblem(res, 404, 'not found');
      const body = (await http.json(req).catch(() => null)) as { key?: unknown } | null;
      if (typeof body?.key !== 'string' || !isSessionKey(body.key)) return sendProblem(res, 400, 'key must be a session key');
      touch.run(body.key, now());
      http.send(res, 200, { ok: true });
    },
    start: () => (prune(), void drain()),
    async stop() {
      unsubscribe();
      await draining;
      db.close();
    },
  };
}
