import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { pendingApprovals, resolveApproval } from '@quack/sdk';
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

// The container contract (RUN-3): GET /ping and POST /invocations, plus the approvals this session waits on; /ws follows.
export function createSessionServer(session: Session) {
  return createServer(async (req, res) => {
    try {
      if (req.method === 'GET' && req.url === '/ping') {
        return send(res, 200, { status: session.busy() ? 'HealthyBusy' : 'Healthy' });
      }
      if (req.method === 'POST' && req.url === '/invocations') {
        const inv = parseInvocation(await readJson(req));
        if (typeof inv === 'string') return send(res, 400, { error: inv });
        const result = await session.submit(inv.input, inv.requestId, inv.wait);
        return send(res, inv.wait ? 200 : 202, result);
      }
      if (req.method === 'GET' && req.url === '/approvals') {
        return send(res, 200, { approvals: await pendingApprovals(session.root, BACKGROUND_CONTEXT) });
      }
      const approval = req.method === 'POST' ? /^\/approvals\/([^/]+)$/.exec(req.url ?? '') : null;
      if (approval) {
        const verdict = parseVerdict(await readJson(req));
        if (typeof verdict === 'string') return send(res, 400, { error: verdict });
        const ok = await resolveApproval(session.root, decodeURIComponent(approval[1]), verdict, BACKGROUND_CONTEXT);
        return send(res, ok ? 200 : 409, ok ? { resolved: true } : { error: 'unknown or already decided' });
      }
      send(res, 404, { error: 'not found' });
    } catch (err) {
      const bad = err instanceof SyntaxError || (err instanceof Error && err.message === 'body too large');
      send(res, bad ? 400 : 500, { error: err instanceof Error ? err.message : String(err) });
    }
  });
}
