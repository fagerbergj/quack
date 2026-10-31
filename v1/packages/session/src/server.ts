import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { pendingApprovals, resolveApproval } from '@quack/sdk';
import { liveUpgrade } from './live.ts';
import type { Session } from './runtime.ts';

const MAX_BODY = 1 << 20;

function send(res: ServerResponse, status: number, body: unknown) {
  res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
}

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

function parseVerdict(body: unknown): { decision: 'approve' | 'reject'; reason?: string; by: string } | string {
  if (typeof body !== 'object' || body === null) return 'body must be a JSON object';
  const { decision, reason, by } = body as Record<string, unknown>;
  if (decision !== 'approve' && decision !== 'reject') return 'decision must be approve or reject';
  if (reason !== undefined && typeof reason !== 'string') return 'reason must be a string';
  if (typeof by !== 'string' || by.length === 0) return 'by must name the approver';
  return { decision, reason, by };
}

type Invocation = { input: string; requestId?: string; wait?: boolean };

function parseInvocation(body: unknown): Invocation | string {
  if (typeof body !== 'object' || body === null) return 'body must be a JSON object';
  const { input, requestId, wait } = body as Record<string, unknown>;
  if (typeof input !== 'string' || input.length === 0) return 'input must be a non-empty string';
  if (requestId !== undefined && typeof requestId !== 'string') return 'requestId must be a string';
  if (wait !== undefined && typeof wait !== 'boolean') return 'wait must be a boolean';
  return { input, requestId, wait };
}

type Route = (session: Session, req: IncomingMessage, match: RegExpExecArray) => Promise<[number, unknown]>;

const ctx = BACKGROUND_CONTEXT;

async function invoke(session: Session, req: IncomingMessage): Promise<[number, unknown]> {
  const inv = parseInvocation(await readJson(req));
  if (typeof inv === 'string') return [400, { error: inv }];
  return [inv.wait ? 200 : 202, await session.submit(inv.input, inv.requestId, inv.wait)];
}

async function decide(session: Session, req: IncomingMessage, match: RegExpExecArray): Promise<[number, unknown]> {
  const verdict = parseVerdict(await readJson(req));
  if (typeof verdict === 'string') return [400, { error: verdict }];
  let id: string;
  try {
    id = decodeURIComponent(match[1]);
  } catch {
    return [400, { error: 'malformed approval id' }];
  }
  const ok = await resolveApproval(session.root, id, verdict, ctx);
  return ok ? [200, { resolved: true }] : [409, { error: 'unknown or already decided' }];
}

// The container contract (RUN-3): GET /ping, POST /invocations and /ws, plus the approvals this session waits on.
const ROUTES: Array<[string, RegExp, Route]> = [
  ['GET', /^\/ping$/, async (s) => [200, { status: s.busy() ? 'HealthyBusy' : 'Healthy' }]],
  ['POST', /^\/invocations$/, invoke],
  ['GET', /^\/approvals$/, async (s) => [200, { approvals: await pendingApprovals(s.root, ctx) }]],
  ['POST', /^\/approvals\/([^/]+)$/, decide],
];

export function createSessionServer(session: Session) {
  const server = createServer(async (req, res) => {
    try {
      for (const [method, pattern, route] of ROUTES) {
        const match = req.method === method ? pattern.exec(req.url ?? '') : null;
        if (match) return send(res, ...(await route(session, req, match)));
      }
      send(res, 404, { error: 'not found' });
    } catch (err) {
      const bad = err instanceof SyntaxError || (err instanceof Error && err.message === 'body too large');
      send(res, bad ? 400 : 500, { error: err instanceof Error ? err.message : String(err) });
    }
  });
  server.on('upgrade', liveUpgrade(session));
  return server;
}
