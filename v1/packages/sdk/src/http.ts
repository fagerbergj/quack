import { timingSafeEqual } from 'node:crypto';
import { createServer, STATUS_CODES, type IncomingMessage, type ServerResponse } from 'node:http';
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
    for await (const chunk of req) {
      size += chunk.length;
      if (size > maxBody) throw new Error('body too large');
      chunks.push(chunk);
    }
    return Buffer.concat(chunks);
  }
  const json = async (req: IncomingMessage): Promise<unknown> => JSON.parse((await readBody(req)).toString('utf8'));
  const authorized = (req: IncomingMessage, token = opts.token ?? '') => safeEqual(req.headers.authorization ?? '', `Bearer ${token}`);
  return { send, problem, readBody, json, authorized };
}

/** Where a session reaches an app, with the launch credential it presents: the distributor that started the session. */
export function appEndpoint(name: string, env: Record<string, string | undefined> = process.env): { url: string; token: string } {
  const { QUACK_INDEX_URL: base, QUACK_INDEX_TOKEN: token } = env;
  if (!base || !token) throw new Error(`QUACK_INDEX_URL and QUACK_INDEX_TOKEN are required to reach the ${name} app`);
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
