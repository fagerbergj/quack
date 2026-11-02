import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { answerQuestion, Artifacts, closeQuestions, failedRows, Failures, pendingApprovals, pendingQuestions, Questions, resolveApproval, serviceHttp } from '@quack/sdk';
import { liveUpgrade } from './live.ts';
import type { Session } from './runtime.ts';

const { send, json: readJson } = serviceHttp();

// The decider comes from the proxy's identity header, never the body; the tailnet port has no header and the tailnet is its auth boundary.
const decider = (req: IncomingMessage): string => String(req.headers['x-authentik-username'] ?? '') || 'tailnet';

function parseVerdict(body: unknown): { decision: 'approve' | 'reject'; reason?: string } | string {
  if (typeof body !== 'object' || body === null) return 'body must be a JSON object';
  const { decision, reason } = body as Record<string, unknown>;
  if (decision !== 'approve' && decision !== 'reject') return 'decision must be approve or reject';
  if (reason !== undefined && typeof reason !== 'string') return 'reason must be a string';
  return { decision, reason };
}

type Invocation = { input: string; requestId?: string; wait?: boolean; whenBusy?: 'followUp'; title?: string; meta?: Record<string, unknown> };

const isMeta = (m: unknown): m is Record<string, unknown> => typeof m === 'object' && m !== null && !Array.isArray(m) && typeof (m as { id?: unknown }).id === 'string';

function parseInvocation(body: unknown): Invocation | string {
  if (typeof body !== 'object' || body === null) return 'body must be a JSON object';
  const { input, requestId, wait, whenBusy, title, meta } = body as Record<string, unknown>;
  if (typeof input !== 'string' || input.length === 0) return 'input must be a non-empty string';
  if (requestId !== undefined && typeof requestId !== 'string') return 'requestId must be a string';
  if (wait !== undefined && typeof wait !== 'boolean') return 'wait must be a boolean';
  if (whenBusy !== undefined && whenBusy !== 'followUp') return 'whenBusy must be followUp';
  if (title !== undefined && typeof title !== 'string') return 'title must be a string';
  if (meta !== undefined && !isMeta(meta)) return 'meta must be an object with a string id';
  return { input, requestId, wait, whenBusy, title, meta };
}

type Route = (session: Session, req: IncomingMessage, match: RegExpExecArray) => Promise<[number, unknown]>;

const ANSWER_STATUS = { ok: 200, 'not-asking': 404, answered: 409, 'bad-choice': 400 } as const;

async function answer(session: Session, req: IncomingMessage, match: RegExpExecArray): Promise<[number, unknown]> {
  const { choice } = (await readJson(req)) as Record<string, unknown> | null ?? {};
  if (typeof choice !== 'string') return [400, { error: 'choice must be a string' }];
  let id: string;
  try {
    id = decodeURIComponent(match[1]);
  } catch {
    return [400, { error: 'malformed question id' }];
  }
  const result = await answerQuestion(session.root, id, choice, decider(req), ctx);
  return [ANSWER_STATUS[result], result === 'ok' ? { answered: true } : { error: result }];
}

const ctx = BACKGROUND_CONTEXT;

async function invoke(session: Session, req: IncomingMessage): Promise<[number, unknown]> {
  const inv = parseInvocation(await readJson(req));
  if (typeof inv === 'string') return [400, { error: inv }];
  if (inv.title) await session.recordTitle(inv.title);
  if (inv.meta) await session.recordMeta(inv.meta);
  return [inv.wait ? 200 : 202, await session.submit(inv.input, inv.requestId, inv.wait, inv.whenBusy)];
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
  const ok = await resolveApproval(session.root, id, { ...verdict, by: decider(req) }, ctx);
  return ok ? [200, { resolved: true }] : [409, { error: 'unknown or already decided' }];
}

// Every sealed version of every kind, with its checker's verdict, oldest first within a kind.
async function artifacts(session: Session): Promise<[number, unknown]> {
  const doc = await session.harness.snapshot(Artifacts, session.root.id, ctx);
  return [200, { artifacts: Object.values(doc?.kinds ?? {}).flatMap((k) => k.versions) }];
}

async function abort(session: Session): Promise<[number, unknown]> {
  await session.root.abort(ctx);
  await closeQuestions(session.root, ctx);
  return [200, { aborted: true }];
}

async function resume(session: Session): Promise<[number, unknown]> {
  const result = await session.resume();
  return result ? [202, result] : [409, { error: 'no failed run to resume' }];
}

// The container contract (RUN-3): GET /ping, POST /invocations and /ws, plus the approvals, questions, failures, artifacts and abort of this session.
const ROUTES: Array<[string, RegExp, Route]> = [
  ['GET', /^\/ping$/, async (s) => {
    const { busy, waitingOnHuman, wakeAt, note } = await s.standing();
    // pid lets a process host tell its own child from a stranger that took the port first.
    return [200, { status: busy ? 'HealthyBusy' : 'Healthy', pid: process.pid, waitingOnHuman, wake: { at: wakeAt }, note: note ?? undefined }];
  }],
  ['POST', /^\/invocations$/, invoke],
  ['GET', /^\/meta$/, async (s) => [200, { meta: await s.meta() }]],
  ['GET', /^\/approvals$/, async (s) => [200, { approvals: await pendingApprovals(s.root, ctx) }]],
  ['GET', /^\/artifacts$/, artifacts],
  ['POST', /^\/abort$/, abort],
  ['POST', /^\/resume$/, resume],
  ['POST', /^\/approvals\/([^/]+)$/, decide],
  ['GET', /^\/questions$/, async (s) => [200, { questions: pendingQuestions(await s.harness.snapshot(Questions, s.root.id, ctx)) }]],
  ['POST', /^\/questions\/([^/]+)$/, answer],
  ['GET', /^\/failures$/, async (s) => [200, { failures: failedRows(await s.harness.snapshot(Failures, s.root.id, ctx)) }]],
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
