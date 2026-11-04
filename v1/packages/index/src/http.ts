import type { IncomingMessage, ServerResponse } from 'node:http';
import { serviceHttp } from '@quack/sdk';
import type { Fact } from './facts.ts';
import type { IndexStore } from './store.ts';

/** Called with each batch of facts the index accepted. */
export type OnFacts = (session: string, facts: Fact[]) => Promise<void>;

const KINDS = new Set(['session', 'artifact', 'approval', 'question', 'failure', 'cost', 'status']);
const SESSION_KEY = /^[A-Za-z0-9:/#._@-]{1,200}$/;

const { send, problem, json: readJson, authorized: bearerOk } = serviceHttp();

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

export const authorized = (req: IncomingMessage, token: string) => bearerOk(req, token);

async function publish(req: IncomingMessage, res: ServerResponse, store: IndexStore, token: string, onFacts?: OnFacts) {
  if (!authorized(req, token)) return problem(res, 401, 'unauthorized');
  const parsed = parseFacts(await readJson(req));
  if (typeof parsed === 'string') return problem(res, 400, parsed);
  await store.upsert(parsed.session, parsed.facts);
  // Sending to a push service must not slow or fail the publish.
  void onFacts?.(parsed.session, parsed.facts).catch((err) => console.error('index: on facts', err));
  send(res, 200, { accepted: parsed.facts.length });
}

const STATES: Record<string, string[]> = {
  '/index/approvals': ['pending', 'resolved'],
  '/index/questions': ['pending', 'answered', 'closed'],
  '/index/failures': ['failed', 'cleared'],
};

const READS: Record<string, [field: string, rows: (store: IndexStore, q: URLSearchParams) => Promise<unknown[]>]> = {
  '/index/sessions': ['sessions', (s) => s.sessions()],
  '/index/artifacts': ['artifacts', (s) => s.artifacts()],
  '/index/approvals': ['approvals', (s, q) => s.approvals(q.get('state') ?? undefined)],
  '/index/questions': ['questions', (s, q) => s.questions(q.get('state') ?? undefined)],
  '/index/failures': ['failures', (s, q) => s.failures(q.get('state') || undefined)], // `||`: an empty state means the default `failed` view
};

const MAX_PAGE = 1000;

// shortcut: the token is an offset into the freshly sorted list, so a fact landing between pages can shift a row; upgrade to keyset if lists outgrow one page in practice.
function pageOf(q: URLSearchParams): { offset: number; size: number } | string {
  const size = Number(q.get('pageSize') || 0);
  if (!Number.isInteger(size) || size < 0) return 'pageSize must be a non-negative integer';
  const token = q.get('pageToken');
  const offset = token ? Number(Buffer.from(token, 'base64url').toString()) : 0;
  if (!Number.isSafeInteger(offset) || offset < 0 || (token && Buffer.from(String(offset)).toString('base64url') !== token)) return 'pageToken is not one this index issued';
  return { offset, size: Math.min(size || MAX_PAGE, MAX_PAGE) };
}

/** Answers a list route, or the problem to send: an unknown `state` or a bad page parameter. */
async function listed(store: IndexStore, path: string, q: URLSearchParams): Promise<{ body: object } | { bad: string }> {
  const state = q.get('state');
  const states = STATES[path];
  if (state && states && !states.includes(state)) return { bad: `state must be one of ${states.join(', ')}` };
  const page = pageOf(q);
  if (typeof page === 'string') return { bad: page };
  const [field, rows] = READS[path]!;
  const all = await rows(store, q);
  const end = page.offset + page.size;
  return { body: { [field]: all.slice(page.offset, end), ...(end < all.length && { nextPageToken: Buffer.from(String(end)).toString('base64url') }) } };
}

// Sessions publish with the service token; reads rely on the proxy for auth. A down database answers 503 and the session retries.
export function indexRoutes(store: IndexStore, token: string, onFacts?: OnFacts) {
  return async (req: IncomingMessage, res: ServerResponse): Promise<boolean> => {
    const url = new URL(req.url ?? '', 'http://distributor');
    const read = req.method === 'GET' && READS[url.pathname];
    const isPublish = req.method === 'POST' && url.pathname === '/index/facts';
    if (!read && !isPublish) return false;
    try {
      if (read) {
        const out = await listed(store, url.pathname, url.searchParams);
        if ('bad' in out) problem(res, 400, out.bad);
        else send(res, 200, out.body);
      } else await publish(req, res, store, token, onFacts);
    } catch (err) {
      const big = err instanceof Error && err.message === 'body too large';
      problem(res, big ? 413 : err instanceof SyntaxError ? 400 : 503, err instanceof Error ? err.message : String(err));
    }
    return true;
  };
}
