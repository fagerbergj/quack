import { timingSafeEqual } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';
import type { Fact } from './facts.ts';
import type { IndexStore } from './store.ts';

const MAX_BODY = 1 << 20;
const KINDS = new Set(['session', 'artifact', 'approval', 'cost', 'status']);
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

/**
 * The index's routes for the distributor: sessions publish with the service token, readers get the rest (the proxy
 * authenticates them for now). Resolves false for any other path. Postgres being down answers 503 and the session retries.
 */
export function indexRoutes(store: IndexStore, token: string) {
  return async (req: IncomingMessage, res: ServerResponse): Promise<boolean> => {
    const url = new URL(req.url ?? '', 'http://distributor');
    if (!url.pathname.startsWith('/index/')) return false;
    try {
      if (req.method === 'POST' && url.pathname === '/index/facts') {
        if (!authorized(req, token)) return send(res, 401, { error: 'unauthorized' }), true;
        const parsed = parseFacts(await readJson(req));
        if (typeof parsed === 'string') return send(res, 400, { error: parsed }), true;
        await store.upsert(parsed.session, parsed.facts);
        return send(res, 200, { accepted: parsed.facts.length }), true;
      }
      if (req.method !== 'GET') return false;
      if (url.pathname === '/index/sessions') return send(res, 200, { sessions: await store.sessions() }), true;
      if (url.pathname === '/index/artifacts') return send(res, 200, { artifacts: await store.artifacts() }), true;
      if (url.pathname === '/index/approvals') return send(res, 200, { approvals: await store.approvals(url.searchParams.get('state') ?? undefined) }), true;
      return false;
    } catch (err) {
      const bad = err instanceof SyntaxError || (err instanceof Error && err.message === 'body too large');
      send(res, bad ? 400 : 503, { error: err instanceof Error ? err.message : String(err) });
      return true;
    }
  };
}
