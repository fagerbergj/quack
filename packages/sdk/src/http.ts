import { timingSafeEqual } from 'node:crypto';
import { createServer, STATUS_CODES, type IncomingMessage, type ServerResponse } from 'node:http';
import { Readable, Writable } from 'node:stream';
import type { App } from './manifest.ts';

/** Constant-time string equality; unequal lengths answer false without comparing. */
export function safeEqual(given: string, expected: string): boolean {
  const a = Buffer.from(given);
  const b = Buffer.from(expected);
  return a.length === b.length && timingSafeEqual(a, b);
}

/** An RFC 9457 problem reply; `error` repeats `detail` so readers that predate problem+json keep working. */
export function sendProblem(res: ServerResponse, status: number, detail: string): void {
  res.writeHead(status, { 'content-type': 'application/problem+json' }).end(JSON.stringify({ type: 'about:blank', title: STATUS_CODES[status] ?? 'Error', status, detail, error: detail }));
}

/** Whether a body read failed on the app's own size limit, which the app answers 413. */
export const tooLarge = (err: unknown) => err instanceof Error && err.message === 'body too large';

/** A server around an in-process app, for tests and dev stacks (the distributor mounts apps itself); `stop` closes it and the app. */
export function serveApp(app: App) {
  const server = createServer((req, res) => void app.handle(req, res));
  return Object.assign(server, { stop: () => (server.close(), app.stop?.()) });
}

/** The scaffolding every extension HTTP service repeats: JSON replies, a bounded body read and a bearer check. */
export function serviceHttp(opts: { token?: string; maxBody?: number } = {}) {
  const maxBody = opts.maxBody ?? 1 << 20;
  const send = (res: ServerResponse, status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
  const problem = sendProblem;
  async function readBody(req: IncomingMessage): Promise<Buffer> {
    let size = 0;
    const chunks: Buffer[] = [];
    for await (const chunk of req.iterator({ destroyOnReturn: false })) {
      size += chunk.length;
      if (size > maxBody) break;
      chunks.push(chunk);
    }
    if (size <= maxBody) return Buffer.concat(chunks);
    // Drop the rest rather than destroy the request: a destroyed one strands its unread body on the kept-alive socket.
    req.resume();
    throw new Error('body too large');
  }
  const json = async (req: IncomingMessage): Promise<unknown> => JSON.parse((await readBody(req)).toString('utf8'));
  const authorized = (req: IncomingMessage, token = opts.token ?? '') => safeEqual(req.headers.authorization ?? '', `Bearer ${token}`);
  return { send, problem, readBody, json, authorized };
}

/** A `fetch` that hands the request to an HTTP handler in this process: no socket, no credential. Only what the sdk's apps and the distributor use of the response API is supported. */
export function inProcessFetch(dispatch: (req: IncomingMessage, res: ServerResponse) => unknown): typeof fetch {
  return async (input, init) => {
    const request = new Request(input as RequestInfo, init);
    const url = new URL(request.url);
    const body = request.body ? Buffer.from(await request.arrayBuffer()) : undefined;
    const req = Object.assign(Readable.from(body ? [body] : []), { method: request.method, url: url.pathname + url.search, headers: Object.fromEntries(request.headers) });
    return new Promise<Response>((resolve, reject) => {
      const chunks: Buffer[] = [];
      const pending: Record<string, string> = {};
      let head: { status: number; headers: Record<string, string> } | undefined;
      const res = Object.assign(new Writable({
        write: (chunk, _enc, done) => (chunks.push(Buffer.from(chunk)), done()),
        final: (done) => {
          const status = head?.status ?? 200;
          resolve(new Response([204, 304].includes(status) ? null : Buffer.concat(chunks), { status, headers: head?.headers }));
          done();
        },
      }), {
        setHeader(name: string, value: string) { pending[name.toLowerCase()] = value; },
        writeHead(status: number, headers: Record<string, string> = {}) {
          head = { status, headers: { ...pending, ...headers } };
          return this;
        },
      });
      Object.defineProperty(res, 'headersSent', { get: () => head !== undefined });
      res.once('error', reject);
      // Aborting must end the call as it would over a socket: stop() paths abort held requests and await them.
      if (request.signal.aborted) return reject(request.signal.reason);
      request.signal.addEventListener('abort', () => void res.destroy(request.signal.reason), { once: true });
      try {
        void Promise.resolve(dispatch(req as unknown as IncomingMessage, res as unknown as ServerResponse)).catch(reject);
      } catch (err) {
        reject(err);
      }
    });
  };
}

let primed: { url: string; token: string } | undefined;

/** Core only: the session runtime hands over, once at boot, the listener URL and launch token it read; undefined clears them. Extensions never call it. */
export function primeSessionEndpoint(e: { url: string; token: string } | undefined): void {
  primed = e;
}

/** Where a session reaches an app, with the launch credential it presents: the distributor that started the session, as the runtime primed it. `env` is a test and development seam, read as-is in place of the primed pair. */
export function appEndpoint(name: string, env?: Record<string, string | undefined>): { url: string; token: string } {
  const { url: base, token } = env ? { url: env.QUACK_INDEX_URL, token: env.QUACK_INDEX_TOKEN } : primed ?? {};
  if (!base || !token) throw new Error(env ? `QUACK_INDEX_URL and QUACK_INDEX_TOKEN are required to reach the ${name} app` : `the ${name} app is reached through the session listener, and this session was launched without QUACK_SESSION_LISTENER_URL and QUACK_LAUNCH_TOKEN_FILE`);
  return { url: `${base.replace(/\/$/, '')}/apps/${name}`, token };
}

export type SessionSubmission = {
  key: string;
  agent: string;
  input: string;
  requestId: string;
  title?: string;
  meta?: object;
  whenBusy?: 'followUp';
  wait?: boolean;
};

/** The distributor's invocation request for a session: the encoded-key URL and the JSON body. */
export function invocationRequest(distributorUrl: string, { key, agent, ...body }: SessionSubmission): { url: string; body: string } {
  return { url: `${distributorUrl}/sessions/${encodeURIComponent(key)}/invocations?agent=${encodeURIComponent(agent)}`, body: JSON.stringify(body) };
}

export function submitToSession(distributorUrl: string, submission: SessionSubmission, doFetch: typeof fetch = fetch): Promise<Response> {
  const { url, body } = invocationRequest(distributorUrl, submission);
  return doFetch(url, { method: 'POST', body });
}
