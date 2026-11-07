// The Sessions API's HTTP adapter (approved v9): the catalog's reads and a person's writes, in the approved wire shapes.
import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendProblem, serviceHttp, tooLarge } from '@quack/sdk';
import type { Catalog, ListRequest } from './core.ts';
import { CursorError } from './keyset.ts';
import type { ArtifactRow, AskRow, Principal, SessionRow } from './port.ts';
import { QueryError } from './query.ts';

/** Who the listener says is calling: a person or the tailnet through the proxy, or a session's own credential. */
export type Caller = Principal | { kind: 'session'; key: string; crossSession: boolean };

/** What a person's write needs beyond the catalog: the session's directory and process, and the session's own gate. */
export type SessionsApiDeps = {
  /** Deletes the session's directory, stopping its process; refused while it works or sleeps until a wake time. */
  deleteSession(key: string): Promise<'deleted' | 'absent' | { refused: string }>;
  /** Hands the answer to the session's gate or question, which waits on it: true, false when the session holds it
   * answered, or the session's reason for refusing a malformed answer. */
  answer(key: string, ask: AskRow, resolution: { by: Principal; choice: string; text?: string; reason?: string }): Promise<boolean | string>;
};

const KEY = /^[A-Za-z0-9:/#._@-]{1,200}$/;
const ASK_ID = /^[A-Za-z0-9._:-]{1,100}$/;
const TITLE_MAX = 120;
const { send, json } = serviceHttp({ maxBody: 64 * 1024 });
const iso = (ms: number) => new Date(ms).toISOString();

class Problem extends Error {
  status: number;
  constructor(status: number, detail: string) {
    super(detail);
    this.status = status;
  }
}

export const sessionWire = (s: SessionRow) => ({
  key: s.key, agent: s.agent, ...(s.title !== undefined && { title: s.title }), state: s.state, createdAt: iso(s.createdAt), lastActivity: iso(s.lastActivity),
  cost: { currencyCode: s.cost.currencyCode, amountMicros: s.cost.amountMicros.toString() }, tokens: s.tokens, pendingAsks: s.pendingAsks,
  ...(s.oldestAskAt !== undefined && { oldestAskAt: iso(s.oldestAskAt) }),
  ...(s.lastRun && { lastRun: { outcome: s.lastRun.outcome, endedAt: iso(s.lastRun.endedAt), failedSteps: s.lastRun.failedSteps } }),
});

export const askWire = (a: AskRow) => ({ ...a, createdAt: iso(a.createdAt), ...(a.resolvedAt !== undefined && { resolvedAt: iso(a.resolvedAt) }) });

// The cross-session list carries metadata; the per-session list carries the content.
export const artifactWire = (a: ArtifactRow, content: boolean) => {
  const { content: body, sealedAt, deliveries, ...rest } = a;
  return { ...rest, ...(content && { content: body }), sealedAt: iso(sealedAt), ...(deliveries && { deliveries: deliveries.map((d) => ({ ...d, at: iso(d.at) })) }) };
};

function listRequest(params: URLSearchParams): ListRequest {
  const size = params.get('pageSize');
  if (size !== null && !/^\d+$/.test(size)) throw new QueryError('pageSize must be an integer from 1 to 1000');
  return { q: params.get('q'), orderBy: params.get('orderBy'), ...(size !== null && { pageSize: Number(size) }), ...(params.get('pageToken') && { pageToken: params.get('pageToken')! }) };
}

const decode = (s: string) => { try { return decodeURIComponent(s); } catch { return undefined; } };
const keyOf = (raw: string) => {
  const key = decode(raw);
  if (!key || !KEY.test(key)) throw new Problem(400, 'invalid session key');
  return key;
};

// A session credential reaches only its own key, and a cross-session list only with the home agent's grant; both are 404, never 403.
function mayRead(caller: Caller, key?: string) {
  if (caller.kind !== 'session') return;
  if (key === undefined ? !caller.crossSession : key !== caller.key) throw new Problem(404, key === undefined ? 'not found' : 'no such session');
}

function writer(caller: Caller): Principal {
  if (caller.kind === 'session') throw new Problem(401, 'a session credential cannot write through the Sessions API');
  return caller;
}

async function body(req: IncomingMessage): Promise<Record<string, unknown>> {
  if (!req.headers['content-type']?.startsWith('application/json')) throw new Problem(415, 'send application/json');
  const parsed = await json(req).catch((err) => { throw new Problem(tooLarge(err) ? 413 : 400, tooLarge(err) ? 'body too large' : 'the body is not JSON'); });
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) throw new Problem(400, 'the body must be an object');
  return parsed as Record<string, unknown>;
}

function patchOf(b: Record<string, unknown>): { title?: string | null; state?: 'archived' | 'stopped' } {
  const { title, state } = b;
  if (title !== undefined && (typeof title !== 'string' || title.length > TITLE_MAX)) throw new Problem(400, `title must be text of at most ${TITLE_MAX} characters`);
  if (state !== undefined && state !== 'archived' && state !== 'stopped') throw new Problem(422, 'state may be written only as archived or stopped');
  return { ...(title !== undefined && { title: (title as string).trim() || null }), ...(state !== undefined && { state }) };
}

function resolutionOf(b: Record<string, unknown>) {
  const r = b.resolution as Record<string, unknown> | undefined;
  if (typeof r !== 'object' || r === null || typeof r.choice !== 'string') throw new Problem(400, 'the body must be { resolution: { choice, text?, reason? } }');
  if ('by' in r) throw new Problem(400, 'resolution.by is set by the server from the credential, never sent');
  for (const k of ['text', 'reason'] as const) if (r[k] !== undefined && typeof r[k] !== 'string') throw new Problem(400, `resolution.${k} must be text`);
  return { choice: r.choice, ...(typeof r.text === 'string' && { text: r.text }), ...(typeof r.reason === 'string' && { reason: r.reason }) };
}

/** The Sessions API's routes over the catalog; false for a request that is not one of them. */
export function sessionsApi(catalog: Catalog, deps: SessionsApiDeps) {
  async function patchSession(req: IncomingMessage, key: string, caller: Caller) {
    const by = writer(caller);
    const change = patchOf(await body(req));
    const row = await catalog.getSession(key);
    if (!row) throw new Problem(404, 'no such session');
    // Checked before any write, so a refused archive leaves the title as it was.
    if (change.state === 'archived' && row.state === 'running') throw new Problem(409, 'a running session cannot be archived');
    if (change.title !== undefined) await catalog.rename(key, change.title);
    if (change.state === 'archived') await catalog.archive(key);
    if (change.state === 'stopped') await catalog.unarchive(key);
    console.log(`session ${key} changed by ${by.kind === 'person' ? by.id : 'the tailnet'}`);
    return sessionWire((await catalog.getSession(key))!);
  }

  async function deleteSession(key: string, caller: Caller) {
    writer(caller);
    if (!(await catalog.getSession(key))) throw new Problem(404, 'no such session');
    const out = await deps.deleteSession(key);
    if (typeof out === 'object') throw new Problem(409, `the session ${out.refused}`);
    await catalog.remove(key);
    return { key };
  }

  async function resolveAsk(req: IncomingMessage, key: string, id: string, caller: Caller) {
    const by = writer(caller);
    const resolution = resolutionOf(await body(req));
    const ask = await catalog.getAsk(key, id);
    if (!ask) throw new Problem(404, 'no such ask');
    const taken = ask.state === 'pending' && (await deps.answer(key, ask, { by, ...resolution }));
    if (typeof taken === 'string') throw new Problem(400, taken);
    if (!taken) throw new Problem(409, `the ask is ${ask.state === 'pending' ? 'already answered' : ask.state}`);
    await catalog.resolve(key, id, { by, ...resolution });
    return askWire((await catalog.getAsk(key, id))!);
  }

  async function one(req: IncomingMessage, key: string, caller: Caller): Promise<unknown> {
    if (req.method === 'PATCH') return patchSession(req, key, caller);
    if (req.method === 'DELETE') return deleteSession(key, caller);
    mayRead(caller, key);
    const row = await catalog.getSession(key);
    if (!row) throw new Problem(404, 'no such session');
    return sessionWire(row);
  }

  async function sub(req: IncomingMessage, url: URL, key: string, rest: string[], caller: Caller): Promise<unknown> {
    if (rest[0] === 'asks' && rest.length === 2) {
      const id = decode(rest[1]!);
      if (!id || !ASK_ID.test(id)) throw new Problem(400, 'invalid ask id');
      if (req.method === 'PATCH') return resolveAsk(req, key, id, caller);
      mayRead(caller, key);
      const ask = await catalog.getAsk(key, id);
      if (!ask) throw new Problem(404, 'no such ask');
      return askWire(ask);
    }
    mayRead(caller, key);
    if (!(await catalog.getSession(key))) throw new Problem(404, 'no such session');
    const req2 = { ...listRequest(url.searchParams), session: key };
    if (rest[0] === 'asks') return paged('asks', await catalog.listAsks(req2), askWire);
    return paged('artifacts', await catalog.listArtifacts(req2), (a) => artifactWire(a, true));
  }

  async function top(url: URL, caller: Caller): Promise<unknown> {
    mayRead(caller);
    const r = listRequest(url.searchParams);
    if (url.pathname === '/sessions') return paged('sessions', await catalog.listSessions(r), sessionWire);
    if (url.pathname === '/asks') return paged('asks', await catalog.listAsks(r), askWire);
    return paged('artifacts', await catalog.listArtifacts(r), (a) => artifactWire(a, false));
  }

  // The methods each route under /sessions/{key} answers.
  const METHODS: Record<string, string[]> = { '': ['GET', 'PATCH', 'DELETE'], asks: ['GET'], 'asks/:id': ['GET', 'PATCH'], artifacts: ['GET'] };

  // Which route a request is, or undefined for one this adapter does not serve.
  function routeOf(req: IncomingMessage, url: URL): ((caller: Caller) => Promise<unknown>) | undefined {
    if (req.method === 'GET' && ['/sessions', '/asks', '/artifacts'].includes(url.pathname)) return (c) => top(url, c);
    const [root, key, ...rest] = url.pathname.split('/').slice(1);
    const shape = rest.length === 2 && rest[0] === 'asks' ? 'asks/:id' : rest.join('/');
    if (root !== 'sessions' || key === undefined || !METHODS[shape]?.includes(req.method ?? '')) return undefined;
    return (c) => (shape === '' ? one(req, keyOf(key), c) : sub(req, url, keyOf(key), rest, c));
  }

  return async (req: IncomingMessage, res: ServerResponse, caller: Caller): Promise<boolean> => {
    const url = new URL(req.url ?? '', 'http://distributor');
    const route = routeOf(req, url);
    if (!route) return false;
    try {
      send(res, 200, await route(caller));
    } catch (err) {
      if (err instanceof Problem) sendProblem(res, err.status, err.message);
      else if (err instanceof QueryError || err instanceof CursorError) sendProblem(res, 400, err.message);
      else sendProblem(res, 503, `the catalog is unavailable: ${err instanceof Error ? err.message : String(err)}`);
    }
    return true;
  };
}

function paged<T>(field: string, page: { rows: T[]; cursor?: string }, wire: (row: T) => unknown) {
  return { [field]: page.rows.map(wire), ...(page.cursor && { nextPageToken: page.cursor }) };
}
