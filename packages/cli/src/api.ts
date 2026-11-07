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
/** The paging every list of the Sessions API shares; `q` is Lucene, so values from people go through escapeTerm. */
export type Page = { q?: string; orderBy?: string; pageSize?: string; pageToken?: string };

const query = (p: Page) => {
  const s = new URLSearchParams(Object.entries(p).filter((e): e is [string, string] => !!e[1])).toString();
  return s ? `?${s}` : '';
};

/** Every row of a list, following nextPageToken to the end, for a read that must be whole. */
export async function whole(get: (p: Page) => Promise<Json>, field: string, p: Page = {}): Promise<Json[]> {
  const rows: Json[] = [];
  for (let pageToken: string | undefined; ;) {
    const page = await get({ ...p, pageToken });
    rows.push(...page[field]);
    if (!page.nextPageToken) return rows;
    pageToken = page.nextPageToken;
  }
}

const SOCKET_MS = 10_000;
const INVOKE_ATTEMPTS = 3;

export function client(root: string, adminToken?: string, socketMs = SOCKET_MS) {
  root = root.replace(/\/+$/, '');
  const path = (key: string) => `sessions/${encodeURIComponent(key)}`;
  const under = (key?: string) => (key ? `${path(key)}/` : '');

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

  return {
    sessions: (p: Page): Promise<Json> => call(`sessions${query(p)}`),
    session: async (key: string): Promise<Json> => call(path(key)),
    agents: async (): Promise<Json[]> => (await call('agents')).agents,
    /** Pending and past asks of one session, or of every session without a key. */
    asks: (p: Page, key?: string): Promise<Json> => call(`${under(key)}asks${query(p)}`),
    artifacts: (p: Page, key?: string): Promise<Json> => call(`${under(key)}artifacts${query(p)}`),
    messages: async (key: string, limit: number): Promise<Array<{ role: string; text: string }>> => (await call(`${path(key)}/messages?limit=${limit}`)).messages,
    exported: async (key: string): Promise<Json> => call(`${path(key)}/export`),
    resolve: (key: string, id: string, resolution: { choice: string; text?: string; reason?: string }) =>
      call(`${path(key)}/asks/${encodeURIComponent(id)}`, 'PATCH', { resolution }),
    abort: (key: string) => call(`${path(key)}/abort`, 'POST'),
    resume: (key: string) => call(`${path(key)}/resume`, 'POST'),
    rename: (key: string, title: string) => call(path(key), 'PATCH', { title }),
    archive: (key: string) => call(path(key), 'PATCH', { state: 'archived' }),
    unarchive: (key: string) => call(path(key), 'PATCH', { state: 'stopped' }),
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
    /** Returns once the server accepts the message; the run is not streamed. A steer goes into the running turn instead of queueing behind it. */
    send: (key: string, input: string, whenBusy?: 'steer') =>
      socket<{ submissionId: string; status: string }>(key, (ws) => ws.send(JSON.stringify({ type: 'input', input, whenBusy })), (f) => (f.type === 'accepted' ? f : undefined)),
    /** False when `id` is not a queued follow-up. The server acknowledges a withdraw only by pushing the queue without it. */
    // shortcut: a follow-up the run takes during the withdraw also leaves the queue and reads as withdrawn; fine until the socket acks withdraws.
    withdraw: (key: string, id: string) => {
      let open: WebSocket;
      let asked = false;
      return socket<boolean>(key, (ws) => (open = ws), (f) => {
        if (f.type !== 'queued') return undefined;
        if (!f.items.some((i: { id: string }) => i.id === id)) return asked;
        if (!asked) open.send(JSON.stringify({ type: 'withdraw', id }));
        asked = true;
        return undefined;
      });
    },
  };
}
