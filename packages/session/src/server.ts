import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import type { Duplex } from 'node:stream';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { answerQuestion, type AnswerInput, Artifacts, failedRows, Failures, pendingApprovals, pendingQuestions, type Principal, Questions, resolveApproval, safeEqual, serviceHttp } from '@quack/sdk';
import { liveUpgrade } from './live.ts';
import type { Session } from './runtime.ts';

const { send, problem, json: readJson } = serviceHttp();
// Routes say { error }; the wire speaks RFC 9457.
const reply = (res: ServerResponse, status: number, body: unknown) => (status >= 400 ? problem(res, status, (body as { error: string }).error) : send(res, status, body));

/** Names who decided on a request from the distributor; the session trusts it only beside the launch token. */
export const PRINCIPAL_HEADER = 'x-quack-principal';
export const principalHeader = (p: Principal) => encodeURIComponent(JSON.stringify(p));

function parsePrincipal(raw: string | string[] | undefined): Principal | undefined {
  try {
    const p = JSON.parse(decodeURIComponent(String(raw))) as Record<string, unknown>;
    if (p?.kind === 'tailnet') return { kind: 'tailnet' };
    return p?.kind === 'person' && typeof p.name === 'string' && p.name ? { kind: 'person', name: p.name } : undefined;
  } catch {
    return undefined;
  }
}

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

// `who` is the principal the distributor named, or undefined when it named none.
type Route = (session: Session, req: IncomingMessage, match: RegExpExecArray, who: Principal | undefined) => Promise<[number, unknown]>;

const NO_PRINCIPAL: [number, unknown] = [400, { error: 'the distributor names who decided' }];

const ANSWER_STATUS = { ok: 200, 'not-asking': 404, answered: 409, 'bad-choice': 400 } as const;

async function answer(session: Session, req: IncomingMessage, match: RegExpExecArray, who: Principal | undefined): Promise<[number, unknown]> {
  if (!who) return NO_PRINCIPAL;
  const body = (await readJson(req)) as Record<string, unknown> | null ?? {};
  const { choice, note, text } = body;
  if ([choice, note, text].some((v) => v !== undefined && typeof v !== 'string') || (choice === undefined && text === undefined)) return [400, { error: 'choice or text must be a string' }];
  let id: string;
  try {
    id = decodeURIComponent(match[1]);
  } catch {
    return [400, { error: 'malformed question id' }];
  }
  const result = await answerQuestion(session.root, id, { choice, note, text } as AnswerInput, who, ctx);
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

async function decide(session: Session, req: IncomingMessage, match: RegExpExecArray, who: Principal | undefined): Promise<[number, unknown]> {
  if (!who) return NO_PRINCIPAL;
  const verdict = parseVerdict(await readJson(req));
  if (typeof verdict === 'string') return [400, { error: verdict }];
  let id: string;
  try {
    id = decodeURIComponent(match[1]);
  } catch {
    return [400, { error: 'malformed approval id' }];
  }
  const ok = await resolveApproval(session.root, id, { ...verdict, by: who }, ctx);
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

// With no principal named, the caller is a service on the distributor's session listener.
async function abort(session: Session, _req: IncomingMessage, _match: RegExpExecArray, who: Principal | undefined): Promise<[number, unknown]> {
  await session.abort(who ?? 'service');
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

/** With `token` (every launch has one), each request and socket must carry it as a bearer, so only the distributor reaches the session and the principal it names decides. */
// Without one (tests), anyone may call and every decision is the tailnet's.
export function createSessionServer(session: Session, opts: { token?: string } = {}) {
  const admitted = (req: IncomingMessage) => opts.token === undefined || safeEqual(req.headers.authorization ?? '', `Bearer ${opts.token}`);
  const who = (req: IncomingMessage): Principal | undefined => (opts.token === undefined ? { kind: 'tailnet' } : parsePrincipal(req.headers[PRINCIPAL_HEADER]));
  const server = createServer(async (req, res) => {
    try {
      if (!admitted(req)) return problem(res, 401, 'unauthorized');
      // Routes match the path; a query only matters to a route that reads it.
      const path = new URL(req.url ?? '', 'http://x').pathname;
      for (const [method, pattern, route] of ROUTES) {
        const match = req.method === method ? pattern.exec(path) : null;
        if (match) return reply(res, ...(await route(session, req, match, who(req))));
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
    if (!admitted(req)) return socket.destroy();
    live.add(socket);
    socket.once('close', () => live.delete(socket));
    upgrade(req, socket, head, who(req));
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
