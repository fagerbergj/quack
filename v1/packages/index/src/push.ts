import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { serviceHttp } from '@quack/sdk';
import webpush from 'web-push';
import type { Fact } from './facts.ts';

export type Subscription = { endpoint: string; keys: { p256dh: string; auth: string } };
type Send = (sub: Subscription, payload: string) => Promise<unknown>;
export type PushOptions = {
  dataDir: string;
  env?: { QUACK_VAPID_PUBLIC_KEY?: string; QUACK_VAPID_PRIVATE_KEY?: string; QUACK_VAPID_SUBJECT?: string };
  /** The session's sealed artifacts, read when a run ends to find versions that will never get a verdict. */
  artifacts?: (session: string) => Promise<Array<{ kind: string; version: number; verdict: boolean }>>;
  /** The session's display title, for the notification; undefined when it has none. */
  title?: (session: string) => Promise<string | undefined>;
  send?: Send;
  now?: () => number;
};

// A fact older than this is a rebuild or a long outage catching up, not something new to wake a phone for.
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

/** What a fact asks of the operator in plain words, or undefined when it asks nothing. */
function ask(f: Fact): { what: string; body?: string } | undefined {
  const d = f.data as Record<string, unknown>;
  if (f.kind === 'approval' && d.state === 'pending') return { what: 'Approval waiting', body: oneLine((d.args as { summary?: unknown } | null)?.summary) || undefined };
  if (f.kind === 'question' && d.state === 'pending') return { what: 'Question waiting', body: oneLine(d.question) };
  if (f.kind === 'failure' && d.state === 'failed') return { what: 'Session failed' };
  return undefined;
}

const isSubscription = (s: unknown): s is Subscription => {
  const x = s as Subscription | null;
  return typeof x?.endpoint === 'string' && x.endpoint.length < 2048 && x.endpoint.startsWith('https://') && typeof x.keys?.p256dh === 'string' && typeof x.keys.auth === 'string';
};

/** Web push: subscriptions and the record of what was already sent live beside the index, in the data dir. */
export function openPush(opts: PushOptions) {
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

  // A report is ready once, when final: a judged version that passed or hit the revision limit, or at the end of a run the newest
  // version of a kind that got no verdict (unjudged kinds, carried-forward approvals). A failed draft that will be revised is never one.
  async function reports(session: string, facts: Fact[]): Promise<Item[]> {
    const item = (kind: string, version: number): Item => ({ key: `${session}\nreport:${kind}:${version}`, what: 'Report ready' });
    const items = facts.flatMap((f) => {
      const v = (f.data as { verdict?: { passed?: boolean; limitReached?: boolean } | null }).verdict;
      return f.kind === 'artifact' && (v?.passed || v?.limitReached) ? [item(String(f.data.kind), Number(f.data.version))] : [];
    });
    if (!facts.some((f) => f.kind === 'status' && (f.data as { state?: string }).state === 'idle')) return items;
    const newest = new Map<string, number>();
    for (const a of (await opts.artifacts?.(session)) ?? []) if (!a.verdict) newest.set(a.kind, Math.max(a.version, newest.get(a.kind) ?? 0));
    return [...items, ...[...newest].map(([kind, version]) => item(kind, version))];
  }

  // The sent row is written before the send, so a crash can lose a push but never repeat one.
  async function notify(session: string, facts: Fact[]) {
    const recent = facts.filter((f) => now() - f.at <= RECENT_MS);
    const asks = recent.flatMap((f): Item[] => {
      const what = ask(f);
      // A failure id is its step, so a later run's failure is a new one; the others are once per id.
      return what ? [{ ...what, key: `${session}\n${f.id}${f.kind === 'failure' ? `@${f.rev}` : ''}` }] : [];
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

  // Like the index reads, relies on the proxy for auth.
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
