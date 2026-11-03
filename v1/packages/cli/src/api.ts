// The distributor's HTTP API as the CLI uses it: the same routes as packages/web/src/api.ts.
export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

export type Client = ReturnType<typeof client>;
type Json = any;

export function client(root: string, adminToken?: string) {
  root = root.replace(/\/+$/, '');
  const path = (key: string) => `sessions/${encodeURIComponent(key)}`;

  async function call(route: string, method = 'GET', body?: object): Promise<Json> {
    const headers = { ...(body && { 'content-type': 'application/json' }), ...(adminToken && { authorization: `Bearer ${adminToken}` }) };
    const res = await fetch(`${root}/${route}`, { method, headers, body: body && JSON.stringify(body) }).catch((e: Error) => {
      throw new ApiError(`cannot reach ${root}: ${e.cause instanceof Error ? e.cause.message : e.message}`, 0);
    });
    const data = await res.json().catch(() => null);
    if (!res.ok) throw new ApiError(data?.error ?? `HTTP ${res.status}`, res.status);
    return data;
  }

  // Resolves with the first frame `want` returns a value for; a close or error before that rejects.
  function socket<T>(key: string, onOpen: ((ws: WebSocket) => void) | undefined, want: (frame: Json) => T | undefined): Promise<T> {
    const url = new URL(`${root}/${path(key)}/ws`);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(url);
      const fail = (m: string) => { ws.close(); reject(new ApiError(m, 0)); };
      ws.onopen = () => onOpen?.(ws);
      ws.onerror = () => fail(`cannot reach ${url.origin}`);
      ws.onclose = () => fail('connection closed before the server answered');
      ws.onmessage = ({ data }) => {
        const frame = JSON.parse(String(data));
        if (frame.type === 'error') return fail(frame.error);
        const got = want(frame);
        if (got === undefined) return;
        ws.onclose = null;
        ws.close();
        resolve(got);
      };
    });
  }

  // The index answers 404 when it is off; say so instead of "not found".
  async function index(name: string, query = ''): Promise<Json[]> {
    try {
      return (await call(`index/${name}${query}`))[name] ?? [];
    } catch (e) {
      throw e instanceof ApiError && e.status === 404 ? new ApiError('the session index is off on this distributor', 1) : e;
    }
  }

  return {
    sessions: async (): Promise<Json[]> => (await call('sessions')).sessions,
    agents: async (): Promise<Json[]> => (await call('agents')).agents,
    approvals: async (key: string): Promise<Json[]> => (await call(`${path(key)}/approvals`)).approvals,
    questions: async (key: string): Promise<Json[]> => (await call(`${path(key)}/questions`)).questions,
    artifacts: async (key: string): Promise<Json[]> => (await call(`${path(key)}/artifacts`)).artifacts,
    index,
    decide: (key: string, id: string, decision: 'approve' | 'reject', reason?: string) =>
      call(`${path(key)}/approvals/${encodeURIComponent(id)}`, 'POST', { decision, ...(reason && { reason }) }),
    abort: (key: string) => call(`${path(key)}/abort`, 'POST'),
    resume: (key: string) => call(`${path(key)}/resume`, 'POST'),
    rename: (key: string, title: string) => call(path(key), 'PATCH', { title }),
    archive: (key: string) => call(path(key), 'PATCH', { archived: true }),
    remove: (key: string) => call(`${path(key)}/delete`, 'POST', {}),
    /** Returns once the server accepts the message; the run is not streamed. */
    send: (key: string, input: string) => socket(key, (ws) => ws.send(JSON.stringify({ type: 'input', input })), (f) => (f.type === 'accepted' ? f : undefined)),
    /** The first frame the socket sends: every entry of the transcript. */
    snapshot: (key: string) => socket<{ entries: Array<{ model: Array<{ role: string; content: unknown }> }> }>(key, undefined, (f) => (f.type === 'snapshot' ? f : undefined)),
  };
}
