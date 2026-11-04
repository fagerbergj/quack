import { timingSafeEqual } from 'node:crypto';
import { STATUS_CODES, type IncomingMessage, type ServerResponse } from 'node:http';

/** Constant-time string equality; unequal lengths answer false without comparing. */
export function safeEqual(given: string, expected: string): boolean {
  const a = Buffer.from(given);
  const b = Buffer.from(expected);
  return a.length === b.length && timingSafeEqual(a, b);
}

/** The scaffolding every extension HTTP service repeats: JSON replies, a bounded body read and a bearer check. */
export function serviceHttp(opts: { token?: string; maxBody?: number } = {}) {
  const maxBody = opts.maxBody ?? 1 << 20;
  const send = (res: ServerResponse, status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
  // RFC 9457 body; `error` repeats `detail` so readers that predate it keep working.
  const problem = (res: ServerResponse, status: number, detail: string) =>
    res.writeHead(status, { 'content-type': 'application/problem+json' }).end(JSON.stringify({ type: 'about:blank', title: STATUS_CODES[status] ?? 'Error', status, detail, error: detail }));
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
