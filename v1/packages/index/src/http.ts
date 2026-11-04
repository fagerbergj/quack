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

const READS: Record<string, (store: IndexStore, q: URLSearchParams) => Promise<unknown>> = {
  '/index/sessions': async (s) => ({ sessions: await s.sessions() }),
  '/index/artifacts': async (s) => ({ artifacts: await s.artifacts() }),
  '/index/approvals': async (s, q) => ({ approvals: await s.approvals(q.get('state') ?? undefined) }),
  '/index/questions': async (s, q) => ({ questions: await s.questions(q.get('state') ?? undefined) }),
  '/index/failures': async (s, q) => ({ failures: await s.failures(q.get('state') || undefined) }),
};

// Sessions publish with the service token; reads rely on the proxy for auth. A down database answers 503 and the session retries.
export function indexRoutes(store: IndexStore, token: string, onFacts?: OnFacts) {
  return async (req: IncomingMessage, res: ServerResponse): Promise<boolean> => {
    const url = new URL(req.url ?? '', 'http://distributor');
    const read = req.method === 'GET' ? READS[url.pathname] : undefined;
    const isPublish = req.method === 'POST' && url.pathname === '/index/facts';
    if (!read && !isPublish) return false;
    const state = url.searchParams.get('state');
    if (read && state && STATES[url.pathname] && !STATES[url.pathname]!.includes(state)) {
      problem(res, 400, `state must be one of ${STATES[url.pathname]!.join(', ')}`);
      return true;
    }
    try {
      if (read) send(res, 200, await read(store, url.searchParams));
      else await publish(req, res, store, token, onFacts);
    } catch (err) {
      const big = err instanceof Error && err.message === 'body too large';
      problem(res, big ? 413 : err instanceof SyntaxError ? 400 : 503, err instanceof Error ? err.message : String(err));
    }
    return true;
  };
}
