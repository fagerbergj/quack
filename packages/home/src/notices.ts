import { DatabaseSync } from 'node:sqlite';
import { escapeTerm, sendProblem, serviceHttp, submitToSession, type App, type AppContext, type RunEnded } from '@quack/sdk';
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

/** Home's app: it records the keys Home's start action returned, and tells home:main, as a follow-up, how each of their runs ended. */
export function homeApp(ctx: Pick<AppContext, 'dbPath' | 'distributorUrl' | 'fetch' | 'subscribe'>): App {
  const db = new DatabaseSync(ctx.dbPath);
  db.exec(`CREATE TABLE IF NOT EXISTS started (key TEXT PRIMARY KEY);
    CREATE TABLE IF NOT EXISTS notices (id TEXT PRIMARY KEY, session TEXT NOT NULL, outcome TEXT NOT NULL, failed TEXT, sent INTEGER NOT NULL DEFAULT 0)`);
  const isStarted = db.prepare('SELECT 1 FROM started WHERE key = ?');
  // Recorded synchronously, before the channel acks the batch, so a crash can repeat a notice's send but never lose it.
  const record = db.prepare('INSERT OR IGNORE INTO notices (id, session, outcome, failed) VALUES (?, ?, ?, ?)');

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

  // One pass at a time; a notice stays unsent until home:main accepts it, and the next event or a restart retries it.
  let draining: Promise<void> | undefined;
  const unsent = db.prepare('SELECT id, session, outcome, failed FROM notices WHERE sent = 0 ORDER BY rowid LIMIT 1');
  async function drainOnce() {
    for (let n = unsent.get() as Notice | undefined; n; n = unsent.get() as Notice | undefined) {
      const res = await submitToSession(ctx.distributorUrl, { ...HOME, input: await text(n), requestId: `home-notice:${n.id}`, whenBusy: 'followUp' }, ctx.fetch);
      if (!res.ok) return console.error(`home: notice for ${n.session} not delivered: HTTP ${res.status}`);
      db.prepare('UPDATE notices SET sent = 1 WHERE id = ?').run(n.id);
    }
  }
  const drain = () => (draining ??= drainOnce().catch((err) => console.error('home: notices:', err)).finally(() => (draining = undefined)));

  // shortcut: a run that ends before the start tool records its key goes unheard; session boot makes that window small, so reconcile with the catalog on record if it shows up.
  const unsubscribe = ctx.subscribe((session, event) => {
    if (event.type !== 'run.ended' || !isStarted.get(session)) return;
    const { outcome, failedSteps } = (event as RunEnded).data;
    const first = failedSteps[0];
    // The event id is per session, so the notice's key carries both.
    record.run(`${session}\n${event.id}`, session, outcome, first ? `${first.step}: ${first.error}` : null);
    void drain();
  });

  return {
    async handle(req, res) {
      if (req.method !== 'POST' || req.url !== '/started') return sendProblem(res, 404, 'not found');
      const body = (await http.json(req).catch(() => null)) as { key?: unknown } | null;
      if (typeof body?.key !== 'string' || !body.key || body.key.length > 512) return sendProblem(res, 400, 'key must be a session key');
      db.prepare('INSERT OR IGNORE INTO started (key) VALUES (?)').run(body.key);
      http.send(res, 200, { ok: true });
    },
    start: () => void drain(),
    async stop() {
      unsubscribe();
      await draining;
      db.close();
    },
  };
}
