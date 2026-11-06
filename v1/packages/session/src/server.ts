import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import type { Duplex } from 'node:stream';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { answerQuestion, type AnswerInput, Artifacts, closeQuestions, failedRows, Failures, pendingApprovals, pendingQuestions, Questions, resolveApproval, serviceHttp } from '@quack/sdk';
import { liveUpgrade } from './live.ts';
import type { Session } from './runtime.ts';

const { send, problem, json: readJson } = serviceHttp();
// Routes say { error }; the wire speaks RFC 9457.
const reply = (res: ServerResponse, status: number, body: unknown) => (status >= 400 ? problem(res, status, (body as { error: string }).error) : send(res, status, body));

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
  const body = (await readJson(req)) as Record<string, unknown> | null ?? {};
  const { choice, note, text } = body;
  if ([choice, note, text].some((v) => v !== undefined && typeof v !== 'string') || (choice === undefined && text === undefined)) return [400, { error: 'choice or text must be a string' }];
  let id: string;
  try {
    id = decodeURIComponent(match[1]);
  } catch {
    return [400, { error: 'malformed question id' }];
  }
  const result = await answerQuestion(session.root, id, { choice, note, text } as AnswerInput, decider(req), ctx);
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

// The whole session as the distributor's export route needs it; it adds the session's metadata.
async function exported(session: Session): Promise<[number, unknown]> {
  const [, { artifacts: sealed }] = await artifacts(session) as [number, { artifacts: unknown[] }];
  return [200, { transcript: await session.transcript(), artifacts: sealed }];
}

async function abort(session: Session): Promise<[number, unknown]> {
  await session.root.abort(ctx);
  await closeQuestions(session.root, ctx);
  return [200, { aborted: true }];
}

async function resume(session: Session): Promise<[number, unknown]> {
  const result = await session.resume();
  if (result === 'busy') return [409, { error: 'session busy: a run is in progress' }];
  return result ? [202, result] : [409, { error: 'no failed run to resume' }];
}

// The container contract (RUN-3): GET /ping, POST /invocations and /ws, plus the messages, approvals, questions, failures, artifacts and abort of this session.
const ROUTES: Array<[string, RegExp, Route]> = [
  ['GET', /^\/ping$/, async (s) => {
    const { busy, waitingOnHuman, wakeAt, note } = await s.standing();
    // pid lets a process host tell its own child from a stranger that took the port first.
    return [200, { status: busy ? 'HealthyBusy' : 'Healthy', pid: process.pid, waitingOnHuman, wake: { at: wakeAt }, note: note ?? undefined }];
  }],
  ['POST', /^\/invocations$/, invoke],
  ['GET', /^\/messages$/, async (s, req) => [200, { messages: await s.messages(Math.min(50, Number(new URL(req.url ?? '', 'http://x').searchParams.get('limit')) || 10)) }]],
  ['GET', /^\/meta$/, async (s) => [200, { meta: await s.meta() }]],
  ['GET', /^\/approvals$/, async (s) => [200, { approvals: await pendingApprovals(s.root, ctx) }]],
  ['GET', /^\/artifacts$/, artifacts],
  ['GET', /^\/export$/, exported],
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
      // Routes match the path; a query only matters to a route that reads it.
      const path = new URL(req.url ?? '', 'http://x').pathname;
      for (const [method, pattern, route] of ROUTES) {
        const match = req.method === method ? pattern.exec(path) : null;
        if (match) return reply(res, ...(await route(session, req, match)));
      }
      problem(res, 404, 'not found');
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      problem(res, message === 'body too large' ? 413 : err instanceof SyntaxError ? 400 : 500, message);
    }
  });
  // Upgraded sockets leave the server's connection tracking, so closeAllConnections misses them and close() would wait on every open page.
  const live = new Set<Duplex>();
  const upgrade = liveUpgrade(session);
  server.on('upgrade', (req, socket, head) => {
    live.add(socket);
    socket.once('close', () => live.delete(socket));
    upgrade(req, socket, head);
  });
  /** Stops listening and ends every connection, live sockets and held invocations included; resolves once the server has closed. */
  const shutdown = () => new Promise<void>((resolve) => {
    server.close(() => resolve());
    server.closeAllConnections();
    for (const socket of live) socket.destroy();
  });
  return Object.assign(server, { shutdown });
}

/** Shuts the server down on SIGTERM or SIGINT, or when the parent holding this process's IPC channel goes away, then runs `cleanup` and exits. */
export function exitOnStop(server: ReturnType<typeof createSessionServer>, cleanup: () => Promise<unknown>) {
  let stopping = false;
  const stop = () => {
    if (stopping) return;
    stopping = true;
    void server.shutdown().then(cleanup).then(() => process.exit(0), (err) => {
      console.error('session shutdown failed:', err);
      process.exit(1);
    });
  };
  for (const signal of ['SIGTERM', 'SIGINT'] as const) process.once(signal, stop);
  // A child that outlived its distributor would share its log with the session process the next distributor starts.
  if (process.channel) process.once('disconnect', stop);
}
