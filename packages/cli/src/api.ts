// The distributor's HTTP API as the CLI uses it: the same routes as packages/web/src/api.ts.
import { randomUUID } from 'node:crypto';

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

export type Client = ReturnType<typeof client>;
type Json = any;
const SOCKET_MS = 10_000;
const INVOKE_ATTEMPTS = 3;

export function client(root: string, adminToken?: string, socketMs = SOCKET_MS) {
  root = root.replace(/\/+$/, '');
  const path = (key: string) => `sessions/${encodeURIComponent(key)}`;

  async function call(route: string, method = 'GET', body?: object): Promise<Json> {
    const headers = { ...(body && { 'content-type': 'application/json' }), ...(adminToken && { authorization: `Bearer ${adminToken}` }) };
    const res = await fetch(`${root}/${route}`, { method, headers, body: body && JSON.stringify(body) }).catch((e: Error) => {
      throw new ApiError(`cannot reach ${root}: ${e.cause instanceof Error ? e.cause.message : e.message}`, 0);
    });
    const data = await res.json().catch(() => null);
    if (!res.ok) throw new ApiError(data?.detail ?? data?.error ?? `HTTP ${res.status}`, res.status);
    return data;
  }

  // Resolves with the first frame `want` returns a value for; a close or error before that rejects.
  // `ms` 0 waits without a deadline, for a stream that ends when the run does.
  function socket<T>(key: string, onOpen: ((ws: WebSocket) => void) | undefined, want: (frame: Json) => T | undefined, ms = socketMs): Promise<T> {
    const url = new URL(`${root}/${path(key)}/ws`);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(url);
      const fail = (m: string) => { clearTimeout(timer); ws.close(); reject(new ApiError(m, 0)); };
      const timer = ms ? setTimeout(() => fail(`cannot reach ${url.origin}: no answer in ${ms / 1000}s`), ms) : undefined;
      ws.onopen = () => onOpen?.(ws);
      ws.onerror = () => fail(`cannot reach ${url.origin}`);
      ws.onclose = () => fail('connection closed before the server answered');
      ws.onmessage = ({ data }) => {
        const frame = JSON.parse(String(data));
        if (frame.type === 'error') return fail(frame.error);
        const got = want(frame);
        if (got === undefined) return;
        clearTimeout(timer);
        ws.onclose = null;
        ws.close();
        resolve(got);
      };
    });
  }

  // The index answers 404 when it is off; say so instead of "not found".
  async function index(name: string, query = ''): Promise<Json[]> {
    try {
      const rows: Json[] = [];
      for (let token = ''; ;) {
        const page = await call(`index/${name}${query}${token && `${query ? '&' : '?'}pageToken=${token}`}`);
        rows.push(...(page[name] ?? []));
        if (!page.nextPageToken) return rows;
        token = page.nextPageToken;
      }
    } catch (e) {
      throw e instanceof ApiError && e.status === 404 ? new ApiError('the session index is off on this distributor', 1) : e;
    }
  }

  return {
    /** Every session matching `filter` (the sessions API's Lucene-style expression); pages are followed to the end. */
    sessions: async (filter?: string): Promise<Json[]> => {
      const rows: Json[] = [];
      for (let token = ''; ;) {
        const page = await call(`sessions?${new URLSearchParams({ pageSize: '1000', ...(filter && { filter }), ...(token && { pageToken: token }) })}`);
        rows.push(...page.sessions);
        if (!page.nextPageToken) return rows;
        token = page.nextPageToken;
      }
    },
    session: async (key: string): Promise<Json> => call(path(key)),
    agents: async (): Promise<Json[]> => (await call('agents')).agents,
    approvals: async (key: string): Promise<Json[]> => (await call(`${path(key)}/approvals`)).approvals,
    questions: async (key: string): Promise<Json[]> => (await call(`${path(key)}/questions`)).questions,
    artifacts: async (key: string): Promise<Json[]> => (await call(`${path(key)}/artifacts`)).artifacts,
    index,
    exported: async (key: string): Promise<Json> => call(`${path(key)}/export`),
    decide: (key: string, id: string, decision: 'approve' | 'reject', reason?: string) =>
      call(`${path(key)}/approvals/${encodeURIComponent(id)}`, 'POST', { decision, ...(reason && { reason }) }),
    abort: (key: string) => call(`${path(key)}/abort`, 'POST'),
    resume: (key: string) => call(`${path(key)}/resume`, 'POST'),
    rename: (key: string, title: string) => call(path(key), 'PATCH', { title }),
    archive: (key: string) => call(path(key), 'PATCH', { archived: true }),
    unarchive: (key: string) => call(path(key), 'PATCH', { archived: false }),
    /** The distributor creates the session on its first invocation. With `wait` the answer is the run's outcome; a long run holds the request open. */
    invoke: async (key: string, input: string, o: { agent?: string; wait?: boolean } = {}): Promise<{ status: string; answer?: string; reason?: string; detail?: string }> => {
      const route = `${path(key)}/invocations${o.agent ? `?agent=${encodeURIComponent(o.agent)}` : ''}`;
      // One id per logical request: a retry after a dropped connection joins the run the first attempt started.
      const body = { input, requestId: `cli:${randomUUID()}`, wait: o.wait };
      for (let attempt = 1; ; attempt++) {
        try {
          return await call(route, 'POST', body);
        } catch (e) {
          if (!(e instanceof ApiError && e.status === 0) || attempt >= INVOKE_ATTEMPTS) throw e;
        }
      }
    },
    /** Calls `onFrame` for every view frame and resolves when the run is over (at once if none is running). */
    follow: (key: string, onFrame: (items: any[] | undefined) => void) =>
      socket<true>(key, undefined, (f) => (f.type !== 'view' ? undefined : (onFrame(f.items), f.live?.run === undefined ? true : undefined)), 0),
    remove: (key: string) => call(path(key), 'DELETE'),
    /** Returns once the server accepts the message; the run is not streamed. */
    send: (key: string, input: string) => socket(key, (ws) => ws.send(JSON.stringify({ type: 'input', input })), (f) => (f.type === 'accepted' ? f : undefined)),
    /** The first frame the socket sends: the newest window of the transcript as items. */
    view: (key: string) => socket<{ items: Array<{ kind: string; entry?: { model?: Array<{ role: string; content: unknown }> } }> }>(key, undefined, (f) => (f.type === 'view' ? f : undefined)),
  };
}
