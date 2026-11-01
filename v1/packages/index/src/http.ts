import { timingSafeEqual } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';
import type { Fact } from './facts.ts';
import type { IndexStore } from './store.ts';

const MAX_BODY = 1 << 20;
const KINDS = new Set(['session', 'artifact', 'approval', 'decision', 'cost', 'status']);
const SESSION_KEY = /^[A-Za-z0-9:/#._@-]{1,200}$/;

const send = (res: ServerResponse, status: number, body: unknown) => void res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));

async function readJson(req: IncomingMessage): Promise<unknown> {
  let size = 0;
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    size += chunk.length;
    if (size > MAX_BODY) throw new Error('body too large');
    chunks.push(chunk);
  }
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

const isFact = (f: unknown): f is Fact => {
  const x = f as Record<string, unknown> | null;
  return typeof x === 'object' && x !== null && typeof x.id === 'string' && x.id.length > 0 && KINDS.has(x.kind as string)
    && Number.isSafeInteger(x.rev) && Number.isSafeInteger(x.at) && typeof x.data === 'object' && x.data !== null;
};

function parseFacts(body: unknown): { session: string; facts: Fact[] } | string {
  const { session, facts } = (body ?? {}) as { session?: unknown; facts?: unknown };
  if (typeof session !== 'string' || !SESSION_KEY.test(session)) return 'session must be a session key';
  if (!Array.isArray(facts) || !facts.every(isFact)) return 'facts must be an array of {id, kind, rev, at, data}';
  return { session, facts };
}

function authorized(req: IncomingMessage, token: string): boolean {
  const given = Buffer.from(req.headers.authorization ?? '');
  const want = Buffer.from(`Bearer ${token}`);
  return given.length === want.length && timingSafeEqual(given, want);
}

async function publish(req: IncomingMessage, res: ServerResponse, store: IndexStore, token: string) {
  if (!authorized(req, token)) return send(res, 401, { error: 'unauthorized' });
  const parsed = parseFacts(await readJson(req));
  if (typeof parsed === 'string') return send(res, 400, { error: parsed });
  await store.upsert(parsed.session, parsed.facts);
  send(res, 200, { accepted: parsed.facts.length });
}

const READS: Record<string, (store: IndexStore, q: URLSearchParams) => Promise<unknown>> = {
  '/index/sessions': async (s) => ({ sessions: await s.sessions() }),
  '/index/artifacts': async (s) => ({ artifacts: await s.artifacts() }),
  '/index/approvals': async (s, q) => ({ approvals: await s.approvals(q.get('state') ?? undefined) }),
  '/index/decisions': async (s, q) => ({ decisions: await s.decisions(q.get('state') ?? undefined) }),
};

// Sessions publish with the service token; reads rely on the proxy for auth. A down database answers 503 and the session retries.
export function indexRoutes(store: IndexStore, token: string) {
  return async (req: IncomingMessage, res: ServerResponse): Promise<boolean> => {
    const url = new URL(req.url ?? '', 'http://distributor');
    const read = req.method === 'GET' ? READS[url.pathname] : undefined;
    const isPublish = req.method === 'POST' && url.pathname === '/index/facts';
    if (!read && !isPublish) return false;
    try {
      if (read) send(res, 200, await read(store, url.searchParams));
      else await publish(req, res, store, token);
    } catch (err) {
      const bad = err instanceof SyntaxError || (err instanceof Error && err.message === 'body too large');
      send(res, bad ? 400 : 503, { error: err instanceof Error ? err.message : String(err) });
    }
    return true;
  };
}
