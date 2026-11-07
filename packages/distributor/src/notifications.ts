import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { serviceHttp } from '@quack/sdk';
import webpush from 'web-push';
import type { CatalogEvent } from '@quack/catalog';

export type Subscription = { endpoint: string; keys: { p256dh: string; auth: string } };
type Send = (sub: Subscription, payload: string) => Promise<unknown>;
export type PushOptions = {
  dataDir: string;
  env?: { QUACK_VAPID_PUBLIC_KEY?: string; QUACK_VAPID_PRIVATE_KEY?: string; QUACK_VAPID_SUBJECT?: string };
  /** The session's sealed versions, read when a run ends: the newest of each kind is the run's report. */
  artifacts?: (session: string) => Promise<Array<{ kind: string; version: number }>>;
  /** The session's display title, for the notification; undefined when it has none. */
  title?: (session: string) => Promise<string | undefined>;
  send?: Send;
  now?: () => number;
};

// An event older than this is a replay or a long outage catching up, not something new to wake a phone for.
const RECENT_MS = 6 * 3600_000;
const { send: reply, problem, json: readJson } = serviceHttp({ maxBody: 16 * 1024 });
const ROUTES = new Set(['GET /push/key', 'POST /push/subscribe', 'POST /push/unsubscribe', 'POST /push/test']);

function vapidKeys(dataDir: string, env: NonNullable<PushOptions['env']>) {
  if (env.QUACK_VAPID_PUBLIC_KEY && env.QUACK_VAPID_PRIVATE_KEY) return { publicKey: env.QUACK_VAPID_PUBLIC_KEY, privateKey: env.QUACK_VAPID_PRIVATE_KEY };
  const file = join(dataDir, 'vapid.json');
  if (existsSync(file)) return JSON.parse(readFileSync(file, 'utf8')) as { publicKey: string; privateKey: string };
  const keys = webpush.generateVAPIDKeys();
  writeFileSync(file, JSON.stringify(keys), { mode: 0o600 });
  return keys;
}

const oneLine = (v: unknown) => (typeof v === 'string' ? v.replace(/\s+/g, ' ').trim().slice(0, 140) : '');

/** What an event asks of the operator in plain words, and the key it is sent once under; undefined when it asks nothing. */
function ask(e: CatalogEvent): { what: string; body?: string; id: string } | undefined {
  if ('extension' in e) return undefined;
  if (e.type === 'ask.opened') {
    const body = e.data.type === 'approval' ? oneLine((e.data.toolCall?.args as { summary?: unknown } | null)?.summary) : oneLine(e.data.prompt?.question);
    return { what: e.data.type === 'approval' ? 'Approval waiting' : 'Question waiting', body: body || undefined, id: `ask:${e.data.id}` };
  }
  if (e.type === 'run.ended' && e.data.outcome === 'failed') return { what: 'Session failed', id: `run:${e.data.run}:failed` };
  return undefined;
}

const isSubscription = (s: unknown): s is Subscription => {
  const x = s as Subscription | null;
  return typeof x?.endpoint === 'string' && x.endpoint.length < 2048 && x.endpoint.startsWith('https://') && typeof x.keys?.p256dh === 'string' && typeof x.keys.auth === 'string';
};

/** Web push, fed by the catalog's applied events: subscriptions and the record of what was sent live in the data dir. */
export function openNotifications(opts: PushOptions) {
  mkdirSync(opts.dataDir, { recursive: true });
  const now = opts.now ?? Date.now;
  const keys = vapidKeys(opts.dataDir, opts.env ?? {});
  const vapidDetails = { subject: opts.env?.QUACK_VAPID_SUBJECT ?? 'https://github.com/fagerbergj/quack', ...keys };
  const send: Send = opts.send ?? ((sub, payload) => webpush.sendNotification(sub, payload, { vapidDetails, TTL: 86_400, urgency: 'high' }));
  const db = new DatabaseSync(join(opts.dataDir, 'push.sqlite'));
  db.exec('create table if not exists subscriptions (endpoint text primary key, data text not null); create table if not exists sent (key text primary key, at integer not null)');

  const subscriptions = () => (db.prepare('select data from subscriptions').all() as Array<{ data: string }>).map((r) => JSON.parse(r.data) as Subscription);
  const forget = (endpoint: string) => db.prepare('delete from subscriptions where endpoint = ?').run(endpoint);

  async function deliver(sub: Subscription, payload: object) {
    try {
      await send(sub, JSON.stringify(payload));
    } catch (err) {
      const status = (err as { statusCode?: number }).statusCode;
      if (status === 404 || status === 410) forget(sub.endpoint);
      else console.error('push failed:', err instanceof Error ? err.message : err);
    }
  }

  type Item = { key: string; what: string; body?: string };

  // A run's end is when its reports are final: the newest version of each kind the session holds, each pushed once.
  async function reports(session: string, events: CatalogEvent[]): Promise<Item[]> {
    if (!events.some((e) => !('extension' in e) && e.type === 'run.ended' && e.data.outcome !== 'failed')) return [];
    const newest = new Map<string, number>();
    for (const a of (await opts.artifacts?.(session)) ?? []) newest.set(a.kind, Math.max(a.version, newest.get(a.kind) ?? 0));
    return [...newest].map(([kind, version]) => ({ key: `${session}\nreport:${kind}:${version}`, what: 'Report ready' }));
  }

  // The sent row is written before the send, so a crash can lose a push but never repeat one.
  async function notify(session: string, events: CatalogEvent[]) {
    const recent = events.filter((e) => now() - e.at <= RECENT_MS);
    const asks = recent.flatMap((e): Item[] => {
      const what = ask(e);
      return what ? [{ what: what.what, ...(what.body && { body: what.body }), key: `${session}\n${what.id}` }] : [];
    });
    for (const it of [...asks, ...(await reports(session, recent))]) {
      if (db.prepare('insert or ignore into sent (key, at) values (?, ?)').run(it.key, now()).changes === 0) continue;
      const name = await opts.title?.(session).catch(() => undefined);
      const payload = { title: name ? `${it.what}: ${name}` : it.what, body: it.body ?? '', url: `#/s/${encodeURIComponent(session)}` };
      await Promise.all(subscriptions().map((s) => deliver(s, payload)));
    }
  }

  async function handle(req: IncomingMessage, res: ServerResponse, path: string): Promise<unknown> {
    if (req.method === 'GET' && path === '/push/key') return reply(res, 200, { publicKey: keys.publicKey });
    const body = (await readJson(req)) as { subscription?: unknown; endpoint?: unknown };
    if (path === '/push/subscribe') {
      if (!isSubscription(body.subscription)) return problem(res, 400, 'subscription must have an https endpoint and keys');
      db.prepare('insert into subscriptions (endpoint, data) values (?, ?) on conflict (endpoint) do update set data = excluded.data').run(body.subscription.endpoint, JSON.stringify(body.subscription));
      return reply(res, 200, { ok: true });
    }
    if (typeof body.endpoint !== 'string') return problem(res, 400, 'endpoint must be a string');
    if (path === '/push/unsubscribe') {
      forget(body.endpoint);
      return reply(res, 200, { ok: true });
    }
    const sub = subscriptions().find((s) => s.endpoint === body.endpoint);
    if (!sub) return problem(res, 404, 'this device is not subscribed');
    await deliver(sub, { title: 'Notifications are on', body: 'You will hear from quack when something needs you.', url: '' });
    reply(res, 200, { ok: true });
  }

  // shortcut: takes no credential though sessions bypass the proxy; the push contract's revision authorizes it.
  const routes = async (req: IncomingMessage, res: ServerResponse): Promise<boolean> => {
    const path = new URL(req.url ?? '', 'http://distributor').pathname;
    if (!path.startsWith('/push/')) return false;
    if (!ROUTES.has(`${req.method} ${path}`)) return problem(res, 404, 'not found'), true;
    try {
      await handle(req, res, path);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      problem(res, message === 'body too large' ? 413 : err instanceof SyntaxError ? 400 : 500, message);
    }
    return true;
  };

  return { notify, routes, close: () => db.close() };
}
