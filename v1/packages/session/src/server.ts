import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { Artifacts, pendingApprovals, resolveApproval } from '@quack/sdk';
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

type Invocation = { input: string; requestId?: string; wait?: boolean; title?: string };

function parseInvocation(body: unknown): Invocation | string {
  if (typeof body !== 'object' || body === null) return 'body must be a JSON object';
  const { input, requestId, wait, title } = body as Record<string, unknown>;
  if (typeof input !== 'string' || input.length === 0) return 'input must be a non-empty string';
  if (requestId !== undefined && typeof requestId !== 'string') return 'requestId must be a string';
  if (wait !== undefined && typeof wait !== 'boolean') return 'wait must be a boolean';
  if (title !== undefined && typeof title !== 'string') return 'title must be a string';
  return { input, requestId, wait, title };
}

/** Questions a person answers, supplied by the extension that asks them (the session package cannot see workflows). */
export type Decisions = {
  pending(): Promise<unknown[]>;
  answer(step: string, choice: string, by: string): Promise<'ok' | 'not-asking' | 'answered' | 'bad-choice'>;
};

type Route = (session: Session, req: IncomingMessage, match: RegExpExecArray, decisions?: Decisions) => Promise<[number, unknown]>;

const ANSWER_STATUS = { ok: 200, 'not-asking': 404, answered: 409, 'bad-choice': 400 } as const;

async function answer(_: Session, req: IncomingMessage, match: RegExpExecArray, decisions?: Decisions): Promise<[number, unknown]> {
  const { choice, by } = (await readJson(req)) as Record<string, unknown> | null ?? {};
  if (typeof choice !== 'string' || typeof by !== 'string' || !by) return [400, { error: 'choice and by must be strings' }];
  let step: string;
  try {
    step = decodeURIComponent(match[1]);
  } catch {
    return [400, { error: 'malformed step name' }];
  }
  const result = await decisions?.answer(step, choice, by) ?? 'not-asking';
  return [ANSWER_STATUS[result], result === 'ok' ? { answered: true } : { error: result }];
}

const ctx = BACKGROUND_CONTEXT;

async function invoke(session: Session, req: IncomingMessage): Promise<[number, unknown]> {
  const inv = parseInvocation(await readJson(req));
  if (typeof inv === 'string') return [400, { error: inv }];
  if (inv.title) await session.recordTitle(inv.title);
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

// Every sealed version of every kind, with its checker's verdict, oldest first within a kind.
async function artifacts(session: Session): Promise<[number, unknown]> {
  const doc = await session.harness.snapshot(Artifacts, session.root.id, ctx);
  return [200, { artifacts: Object.values(doc?.kinds ?? {}).flatMap((k) => k.versions) }];
}

async function abort(session: Session): Promise<[number, unknown]> {
  await session.root.abort(ctx);
  return [200, { aborted: true }];
}

// The container contract (RUN-3): GET /ping, POST /invocations and /ws, plus the approvals, artifacts and abort of this session.
const ROUTES: Array<[string, RegExp, Route]> = [
  ['GET', /^\/ping$/, async (s) => {
    const { busy, waitingOnHuman, wakeAt, note } = await s.standing();
    return [200, { status: busy ? 'HealthyBusy' : 'Healthy', waitingOnHuman, wake: { at: wakeAt }, note: note ?? undefined }];
  }],
  ['POST', /^\/invocations$/, invoke],
  ['GET', /^\/approvals$/, async (s) => [200, { approvals: await pendingApprovals(s.root, ctx) }]],
  ['GET', /^\/artifacts$/, artifacts],
  ['POST', /^\/abort$/, abort],
  ['POST', /^\/approvals\/([^/]+)$/, decide],
  ['GET', /^\/decisions$/, async (_s, _r, _m, d) => [200, { decisions: (await d?.pending()) ?? [] }]],
  ['POST', /^\/decisions\/([^/]+)$/, answer],
];

export function createSessionServer(session: Session, decisions?: Decisions) {
  const server = createServer(async (req, res) => {
    try {
      for (const [method, pattern, route] of ROUTES) {
        const match = req.method === method ? pattern.exec(req.url ?? '') : null;
        if (match) return send(res, ...(await route(session, req, match, decisions)));
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
