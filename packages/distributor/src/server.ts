import { createServer, request, type IncomingMessage, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { connect } from 'node:net';
import { extname, resolve, sep } from 'node:path';
import type { Duplex } from 'node:stream';
import { sessionsApi, type Caller, type Catalog } from '@quack/catalog';
import { principalName, safeEqual, sendProblem, serviceHttp, type Principal } from '@quack/sdk';
import { PRINCIPAL_HEADER, principalHeader } from '@quack/session';
import { sessionToken } from './credentials.ts';
import { AgentError, type HostDriver } from './process-driver.ts';
import { sessionRefusal, type SessionCaller } from './session-access.ts';

const KEY = /^[A-Za-z0-9:/#._@-]{1,200}$/;

export const { readBody, send, json } = serviceHttp();
function safeDecode(s: string): string | undefined {
  try {
    return decodeURIComponent(s);
  } catch {
    return undefined;
  }
}

const TYPES: Record<string, string> = {
  '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.json': 'application/json', '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon', '.webmanifest': 'application/manifest+json','.map': 'application/json',
};

// Serves a file under root for a GET path; false when the path escapes root or names no file.
async function serveStatic(root: string, pathname: string, req: IncomingMessage, res: ServerResponse): Promise<boolean> {
  const rel = safeDecode(pathname);
  if (rel === undefined || rel.includes('\0')) return false;
  const file = resolve(root, '.' + (rel.endsWith('/') ? rel + 'index.html' : rel));
  if (!file.startsWith(resolve(root) + sep)) return false;
  try {
    const data = await readFile(file);
    // Only assets/ is content-hashed; everything else (index.html, manifest, icons) must revalidate.
    const hashed = file.startsWith(resolve(root, 'assets') + sep);
    const etag = `"${createHash('sha1').update(data).digest('base64url')}"`;
    const headers = { etag, 'cache-control': hashed ? 'public, max-age=31536000, immutable' : 'no-cache' };
    if (req.headers['if-none-match'] === etag) res.writeHead(304, headers).end();
    else res.writeHead(200, { ...headers, 'content-type': TYPES[extname(file)] ?? 'application/octet-stream' }).end(data);
    return true;
  } catch {
    return false;
  }
}

// Static files answer GETs that are not API routes, and only when a web dir is configured.
async function serveWeb(root: string | undefined, req: IncomingMessage, res: ServerResponse): Promise<boolean> {
  if (!root || req.method !== 'GET') return false;
  const { pathname } = new URL(req.url ?? '', 'http://distributor');
  return !/^\/(sessions|agents|version)(\/|$)/.test(pathname) && serveStatic(root, pathname, req, res);
}

// Routes /sessions/<url-encoded key>/<path> to the one process that owns that session (DSP-1..4). Holds no domain logic.
function sessionPath(url: string | undefined): { key: string; path: string; search: string; agent?: string } | undefined {
  const { pathname, searchParams } = new URL(url ?? '', 'http://distributor');
  const match = /^\/sessions\/([^/]+)(\/.*)$/.exec(pathname);
  if (!match) return undefined;
  const key = safeDecode(match[1]);
  if (!key || !KEY.test(key)) return undefined;
  const agent = searchParams.get('agent') || undefined;
  searchParams.delete('agent');
  const search = searchParams.size ? `?${searchParams}` : '';
  return { key, path: match[2], search, agent };
}

// Splices a WebSocket upgrade onto the owning session's /ws; the session process speaks the protocol.
async function proxyUpgrade(target: string, auth: Record<string, string>, req: IncomingMessage, path: string, client: Duplex, head: Buffer) {
  const { hostname, port } = new URL(target);
  const upstream = connect(Number(port), hostname);
  await new Promise((resolve, reject) => upstream.once('connect', resolve).once('error', reject));
  const pairs = req.rawHeaders.flatMap((v, i) => (i % 2 ? [] : [[v, req.rawHeaders[i + 1]!]])).filter(([name]) => !NOT_FORWARDED.has(name.toLowerCase()));
  const headers = [...pairs, ...Object.entries(auth)].map(([name, v]) => `${name}: ${v}\r\n`).join('');
  upstream.write(`GET ${path} HTTP/1.1\r\n${headers}\r\n`);
  if (head.length) upstream.write(head);
  upstream.pipe(client).pipe(upstream);
  const end = () => { upstream.destroy(); client.destroy(); };
  upstream.on('error', end).on('close', end);
  client.on('error', end).on('close', end);
}

/** The header the reverse proxy adds with the deployment's proxy secret, so a local process cannot pass off an identity header as the proxy's. */
export const PROXY_PROOF = 'x-quack-proxy-secret';

// A caller's own credential, identity or principal never reaches a session; the distributor sets them.
const NOT_FORWARDED = new Set(['authorization', 'x-authentik-username', PROXY_PROOF, PRINCIPAL_HEADER]);

// node:http, not fetch: undici gives up on headers after 300 s, and a wait:true invocation sends none until its run settles.
// A client that hangs up aborts the upstream request.
function forward(url: string, method: string, body: Uint8Array | undefined, res: ServerResponse, auth: Record<string, string>): Promise<void> {
  return new Promise((resolve, reject) => {
    const up = request(url, { method, headers: { 'content-type': 'application/json', ...auth } }, (r) => {
      res.writeHead(r.statusCode ?? 502, { 'content-type': r.headers['content-type'] ?? 'application/json' });
      r.pipe(res).on('finish', resolve);
      r.on('error', (err) => (res.destroyed || res.writableFinished ? resolve() : reject(err)));
    });
    res.on('close', () => { if (!res.writableFinished) up.destroy(); });
    up.on('error', (err) => (res.headersSent || res.destroyed ? resolve() : reject(err)));
    up.end(body);
  });
}

/** The caller on the public listener: the person the proxy names, when the request carries the proxy's proof, else the tailnet; undefined for an identity nobody vouched for. */
function principalOf(req: IncomingMessage, secret: string | undefined): Principal | undefined {
  const name = req.headers['x-authentik-username'];
  if (name === undefined) return { kind: 'tailnet' };
  const proof = req.headers[PROXY_PROOF];
  return secret && typeof proof === 'string' && safeEqual(proof, secret) && typeof name === 'string' && name ? { kind: 'person', name } : undefined;
}

function errorStatus(err: unknown): number {
  if (err instanceof AgentError) return err.status;
  return err instanceof Error && err.message === 'body too large' ? 400 : 502;
}

type SetTimer = (fn: () => void, ms: number) => () => void;
const realTimer: SetTimer = (fn, ms) => {
  const t = setTimeout(fn, ms);
  t.unref();
  return () => clearTimeout(t);
};
const MAX_TIMER_MS = 2 ** 31 - 1;
// A session that cannot answer this fast is left for the next sweep, so one stuck process cannot stall the sweep and every wake behind it.
export const PING_TIMEOUT_MS = 2_000;
// A failed wake is retried after 1 s, doubling to at most this.
const WAKE_RETRY_MAX_MS = 5 * 60_000;

type SessionAuth = { sessionOf(token: string): string | undefined; crossSession(key: string): Promise<boolean>; events?: (req: IncomingMessage, res: ServerResponse, key: string) => Promise<boolean> };
// The Sessions API's caller: the catalog's Principal names a person by id, the session's by name.
const callerOf = (who: Principal): Caller => (who.kind === 'person' ? { kind: 'person', id: who.name } : who);

type Ping = { status: string; waitingOnHuman?: boolean; wake?: { at?: number }; note?: string | null };

// `now` and `setTimer` are injectable so tests run the idle sweep and wake timers on a fake clock.
export function createDistributor(driver: HostDriver, opts: { idleMs?: number; webDir?: string; /** Routes beside the sessions': web push, mounted apps, the credential broker. */ routes?: (req: IncomingMessage, res: ServerResponse) => Promise<boolean>; /** The proxy's secret: an identity header counts only beside it in PROXY_PROOF, and without one it never does. */ proxySecret?: string; /** Who a launch credential on the session listener belongs to, whether that session may reach others, and the lifecycle channel it reports on. */ sessions?: SessionAuth; /** The session catalog the Sessions API serves. */ catalog?: Catalog; /** Peers the public listener drops at connect, such as session containers' network. */ refuse?: (address: string | undefined) => boolean; now?: () => number; setTimer?: SetTimer } = {}) {
  const now = opts.now ?? Date.now;
  const setTimer = opts.setTimer ?? realTimer;
  const lastUse = new Map<string, number>();
  const stopping = new Map<string, Promise<void>>();
  const holds = new Map<string, number>();
  // Each session's proxied page sockets, so a delete can end them: a session process does not stop while one is open.
  const sockets = new Map<string, Set<Duplex>>();
  // A delete in flight per key; requests for that key wait it out, then start a fresh session.
  const removing = new Map<string, Promise<void>>();
  // Requests per key past that wait: one may be starting or calling the process, so a delete is refused until it ends.
  const serving = new Map<string, number>();
  // Whether each running session was mid-run at its last sweep; a listing reads this and never pings.
  const active = new Map<string, boolean>();
  const notes = new Map<string, string>();
  const wakeTimers = new Map<string, () => void>();
  // Sessions whose busy marker (a wake time of "now") is on disk; written by the sweep so a crash still restarts them.
  const marked = new Set<string>();
  // Consecutive failed wakes per session, for the retry backoff and so a failure streak is logged once.
  const wakeFailures = new Map<string, number>();
  let draining = false;
  const idleMs = opts.idleMs ?? 10 * 60_000;
  const idle = (key: string) => !holds.get(key) && now() - (lastUse.get(key) ?? 0) >= idleMs;
  const hold = (key: string, by: number) => holds.set(key, (holds.get(key) ?? 0) + by);
  // The session's server takes only the distributor's bearer, and a decision only the principal named beside it.
  const upstream = (key: string, who?: Principal): Record<string, string> => {
    const token = driver.credential(key);
    return { ...(token && { authorization: `Bearer ${token}` }), ...(who && { [PRINCIPAL_HEADER]: principalHeader(who) }) };
  };

  // Sweeps and wake-ups run one at a time; a caller awaiting `maintain` also waits out those queued before it.
  let queue: Promise<void> = Promise.resolve();
  const maintain = (fn: () => Promise<void> = async () => {}) => (queue = queue.then(fn, fn));

  // A stopped session that sleeps is restarted at its wake time (NFR-PORT-3); a long wait re-arms in slices since timers cap near 24 days.
  function arm(key: string, at: number | undefined) {
    wakeTimers.get(key)?.();
    wakeTimers.delete(key);
    if (at === undefined || draining) return;
    wakeTimers.set(key, setTimer(() => {
      wakeTimers.delete(key);
      if (now() < at) return arm(key, at);
      void maintain(() => wake(key));
    }, Math.min(Math.max(0, at - now()), MAX_TIMER_MS)));
  }

  // A due wake is retried with backoff until it succeeds or the session is gone.
  function retryWake(key: string, why: string) {
    const failures = (wakeFailures.get(key) ?? 0) + 1;
    wakeFailures.set(key, failures);
    if (failures === 1) console.warn(`session ${key}: ${why}; retrying with backoff`);
    arm(key, now() + Math.min(1000 * 2 ** (failures - 1), WAKE_RETRY_MAX_MS));
  }

  async function wake(key: string) {
    await stopping.get(key)?.catch(() => {});
    await removing.get(key);
    try {
      if ((await driver.agentOf(key)) === undefined) return void wakeFailures.delete(key);
      await driver.ensure(key);
      // A grace period, so the next sweep does not stop the session before it has run.
      lastUse.set(key, now());
      // A busy session keeps its marker, so a crash before its next sweep still restarts it.
      if (!marked.has(key)) await driver.setWake(key, undefined);
    } catch (err) {
      retryWake(key, `wake failed: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  async function ping(key: string): Promise<Ping> {
    const url = await driver.ensure(key);
    const deadline = new AbortController();
    const cancel = setTimer(() => deadline.abort(new Error(`session ${key} did not answer /ping within ${PING_TIMEOUT_MS} ms`)), PING_TIMEOUT_MS);
    try {
      return (await (await fetch(`${url}/ping`, { headers: upstream(key), signal: deadline.signal })).json()) as Ping;
    } finally {
      cancel();
    }
  }

  // Records the session's earliest sleep before stopping, so a restarted distributor still wakes it.
  async function stopSession(key: string, at: number | undefined) {
    active.delete(key);
    notes.delete(key);
    marked.delete(key);
    wakeFailures.delete(key);
    const stop = driver.stop(key).finally(() => stopping.delete(key));
    stopping.set(key, stop);
    await driver.setWake(key, at);
    arm(key, at);
    await stop;
  }

  // Busy sessions are recorded as due now, like wake timers, so a restart resumes them; once idle the record reverts to the session's own sleep.
  async function mark(key: string, busy: boolean, sleepAt: number | undefined) {
    if (busy && !marked.has(key)) {
      await driver.setWake(key, now());
      marked.add(key);
    } else if (!busy && marked.delete(key)) {
      wakeFailures.delete(key);
      await driver.setWake(key, sleepAt);
    }
  }

  // A session last seen busy whose process is gone (it crashed past the driver's restart budget, or its restart failed) is due now.
  function rewakeLost() {
    const running = new Set(driver.running());
    for (const key of marked) if (!running.has(key) && !stopping.has(key) && !wakeTimers.has(key)) retryWake(key, 'its process exited mid-run');
  }

  async function sweepOnce() {
    for (const key of driver.running()) {
      try {
        const { status, wake, waitingOnHuman, note } = await ping(key);
        notes.set(key, note ?? '');
        active.set(key, status === 'HealthyBusy' || Boolean(waitingOnHuman));
        // A run with no log messages (a workflow) still counts as activity while it works.
        if (status === 'HealthyBusy') await driver.touch(key, now());
        await mark(key, status === 'HealthyBusy', wake?.at);
        if (!idle(key)) continue;
        // Re-checked after the ping, and requests wait out a stop in flight, so none reaches a dying process.
        if (status !== 'Healthy' || !idle(key)) continue;
        await stopSession(key, wake?.at);
      } catch {
        // a session that cannot answer /ping is left for the next sweep
      }
    }
    rewakeLost();
  }

  let sweepStop = () => {};
  function scheduleSweep() {
    sweepStop = setTimer(() => { void maintain(sweepOnce).then(scheduleSweep); }, Math.max(50, Math.min(idleMs, 30_000)));
  }
  scheduleSweep();
  void maintain(() => driver.wakes().then((all) => all.forEach(({ key, at }) => arm(key, at)), () => {}));

  // Stops every running session, keeping each one's wake time for the next distributor; active work wakes at once, so a deploy does not strand it.
  async function drain() {
    draining = true;
    const wakeFor = (p: Ping) => (p.status === 'HealthyBusy' ? now() : p.wake?.at);
    // A session that cannot answer keeps the marker its last sweep wrote.
    await Promise.all(driver.running().map(async (key) => stopSession(key, await ping(key).then(wakeFor, () => (marked.has(key) ? now() : undefined)))));
  }

  // A list view polls approvals, so reading them must not keep a session warm or start an idle one;
  // a stopped session has none to show here (its store is not read) until it wakes. shortcut: the session index replaces this.
  async function peekApprovals(key: string, search: string, res: ServerResponse) {
    if ((await driver.agentOf(key)) === undefined) return sendProblem(res, 404, 'no such session');
    if (!driver.running().includes(key)) return send(res, 200, { approvals: [] });
    const found = await fetch(`${await driver.ensure(key)}/approvals${search}`, { headers: upstream(key) });
    res.writeHead(found.status, { 'content-type': found.headers.get('content-type') ?? 'application/json' });
    res.end(Buffer.from(await found.arrayBuffer()));
  }

  // One self-contained file: the session's metadata beside its transcript and sealed artifacts, as a download.
  function sendExport(session: { key: string }, exported: object | undefined, res: ServerResponse) {
    if (!exported) return sendProblem(res, 502, 'session export failed');
    const name = `quack-${session.key.replace(/[^A-Za-z0-9._-]+/g, '-')}.json`;
    res.writeHead(200, { 'content-type': 'application/json', 'content-disposition': `attachment; filename="${name}"` });
    res.end(JSON.stringify({ session, ...exported }, null, 2));
  }

  // Only a submission starts a session; any other request for an unknown or deleted key is a 404 that creates nothing.
  async function mustExist(key: string, what: string) {
    if (what !== 'POST /invocations' && (await driver.agentOf(key)) === undefined) throw new AgentError(404, 'no such session');
  }

  // A request in flight keeps its session from idling out, however long it waits on the run.
  async function withSession(key: string, agent: string | undefined, what: string, use: (url: string) => Promise<void>) {
    lastUse.set(key, now());
    hold(key, 1);
    let admitted = false;
    try {
      await stopping.get(key);
      await removing.get(key);
      serving.set(key, (serving.get(key) ?? 0) + 1);
      admitted = true;
      await mustExist(key, what);
      await switchRole(key, agent, what);
      const url = await driver.ensure(key, agent);
      arm(key, undefined);
      await use(url);
    } finally {
      if (admitted) serving.set(key, serving.get(key)! - 1);
      if (serving.get(key) === 0) serving.delete(key);
      hold(key, -1);
      lastUse.set(key, now());
    }
  }

  // A read never starts a process: a running session answers from its process, a stopped one from its store.
  async function exportRoute(key: string, agent: string | undefined, res: ServerResponse) {
    await stopping.get(key)?.catch(() => {});
    await removing.get(key);
    const [bound, row] = await Promise.all([driver.agentOf(key), opts.catalog?.getSession(key)]);
    if (bound === undefined || !row) return sendProblem(res, 404, 'no such session');
    // Title, archive and times live in the catalog; the binding and pinned version are the driver's.
    const info = { key, agent: bound, ...(await driver.pinned(key)), ...(row.title !== undefined && { title: row.title }), ...(row.state === 'archived' && { archived: true }), createdAt: new Date(row.createdAt).toISOString(), lastActivity: new Date(row.lastActivity).toISOString() };
    if (driver.running().includes(key)) {
      return withSession(key, agent, 'GET /export', async (url) => {
        const found = await fetch(`${url}/export`, { headers: upstream(key) });
        sendExport(info, found.ok ? ((await found.json()) as object) : undefined, res);
      });
    }
    if (agent && agent !== info.agent) throw new AgentError(409, `session runs agent ${info.agent}, not ${agent}`);
    sendExport(info, await driver.exported(key).catch((err) => void console.error(`session ${key}: export from its store failed: ${err.message}`)), res);
  }

  async function listing(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const { pathname } = new URL(req.url ?? '', 'http://distributor');
    if (req.method !== 'GET' || !['/agents', '/version'].includes(pathname)) return false;
    if (pathname === '/version') send(res, 200, { commit: process.env.QUACK_COMMIT || null, builtAt: process.env.QUACK_BUILT_AT || null });
    else send(res, 200, { agents: await driver.agents() });
    return true;
  }

  // One agent's definition at a version, and promotion of its current draft; same boundary as housekeeping.
  async function agentRoutes(req: IncomingMessage, res: ServerResponse, who: Principal | undefined): Promise<boolean> {
    const match = /^\/agents\/([^/?]+)(\/promote|\?version=(\d+))?$/.exec(req.url ?? '');
    const name = match && safeDecode(match[1]!);
    if (!name || (req.method === 'GET' ? match[2]?.startsWith('/') : match[2] !== '/promote')) return false;
    if (req.method === 'GET') {
      const found = await driver.agent(name, match[3] ? Number(match[3]) : undefined);
      return (found ? send(res, 200, found) : sendProblem(res, 404, 'no such agent version')), true;
    }
    if (!req.headers['content-type']?.startsWith('application/json')) return sendProblem(res, 415, 'send JSON'), true;
    const { version } = ((await json(req).catch(() => null)) ?? {}) as { version?: unknown };
    if (typeof version !== 'number') return sendProblem(res, 400, 'version must be a number'), true;
    const status = await driver.promote(name, version);
    console.log(`agent ${name} version ${version} promoted by ${who ? principalName(who) : 'tailnet'}`);
    send(res, 200, { name, version, status });
    return true;
  }

  // Mechanism only: which sessions are worth deleting is the caller's policy.
  async function removeSession(key: string): Promise<'deleted' | 'absent' | { refused: string }> {
    let done = () => {};
    removing.set(key, new Promise<void>((r) => (done = r)));
    try {
      return await removeChecked(key);
    } finally {
      removing.delete(key);
      done();
    }
  }

  // Only work refuses a delete: a run in flight, a sleep with a wake time, a person's pending decision. An idle process, or a page watching it, is stopped first.
  // Marked in `removing` before the first await, so new requests wait for the delete; ones already proxied end when the process stops.
  async function removeChecked(key: string): Promise<'deleted' | 'absent' | { refused: string }> {
    await stopping.get(key);
    const live = driver.running().includes(key);
    const busy = live && (await ping(key).then((p) => p.status === 'HealthyBusy', () => true));
    const why = busy ? 'is running'
      : serving.has(key) ? 'is handling a request'
      : !marked.has(key) && (await driver.wakes()).some((w) => w.key === key) ? 'is sleeping until its wake time'
        : ((await opts.catalog?.getSession(key))?.pendingAsks ?? 0) > 0 ? 'is waiting on a person' : undefined;
    if (why) return { refused: why };
    for (const s of sockets.get(key) ?? []) s.destroy();
    if (live) await driver.stop(key);
    marked.delete(key);
    wakeFailures.delete(key);
    if (!(await driver.remove(key))) return 'absent';
    arm(key, undefined);
    active.delete(key);
    notes.delete(key);
    console.log(`session ${key} deleted`);
    return 'deleted';
  }

  // A free answer arrives as text (its choice the same text, not an option); a reason is the session's note.
  const questionAnswer = (ask: { prompt?: { options: string[] } }, r: { choice: string; text?: string; reason?: string }) => ({
    ...(r.text === undefined || ask.prompt?.options.includes(r.choice) ? { choice: r.choice } : {}),
    ...(r.text !== undefined && { text: r.text }), ...(r.reason !== undefined && { note: r.reason }),
  });

  // A person's answer goes to the session's own gate or question, which is what the waiting run reads.
  async function answer(key: string, ask: { id: string; type: 'approval' | 'question'; prompt?: { options: string[] } }, r: { by: { kind: 'person'; id: string } | { kind: 'tailnet' }; choice: string; text?: string; reason?: string }): Promise<boolean | string> {
    const who: Principal = r.by.kind === 'person' ? { kind: 'person', name: r.by.id } : r.by;
    const [path, body] = ask.type === 'approval'
      ? [`/approvals/${encodeURIComponent(ask.id)}`, { decision: r.choice, ...(r.reason !== undefined && { reason: r.reason }) }]
      : [`/questions/${encodeURIComponent(ask.id)}`, questionAnswer(ask, r)];
    let out: boolean | string = false;
    await withSession(key, undefined, `POST ${path}`, async (url) => {
      const res = await fetch(url + path, { method: 'POST', headers: { 'content-type': 'application/json', ...upstream(key, who) }, body: JSON.stringify(body) });
      const reply = (await res.json().catch(() => ({}))) as { error?: string };
      out = res.ok || (res.status === 400 ? reply.error ?? 'the session refused the answer' : false);
    });
    return out;
  }
  const api = opts.catalog && sessionsApi(opts.catalog, { deleteSession: removeSession, answer });

  // A submission naming another agent changes the session's role if no run is active; an open run, a pending approval or a sleep is still a run.
  // shortcut: a busy session is refused (409) rather than queued as the next run; queue it if callers need follow-ups across roles.
  async function switchRole(key: string, agent: string | undefined, request: string) {
    if (!agent || request !== 'POST /invocations') return;
    const recorded = await driver.agentOf(key);
    if (recorded === undefined || recorded === agent) return;
    const { status, waitingOnHuman, wake } = await ping(key);
    if (status !== 'Healthy' || waitingOnHuman || wake?.at !== undefined) throw new AgentError(409, `session is mid-run as agent ${recorded}; ${agent} can start once it finishes`);
    await driver.rebind(key, agent);
    await stopSession(key, undefined);
  }


  // `who` is the public caller's principal; a session caller has none, and the session listener lets it reach no route that would read one.
  // The routes the distributor answers itself, before a request is forwarded to a session's process.
  const ownRoute = async (req: IncomingMessage, res: ServerResponse, who: Principal | undefined) =>
    (!!who && (await api?.(req, res, callerOf(who)))) || (await listing(req, res)) || (await agentRoutes(req, res, who)) || (await opts.routes?.(req, res)) || (await serveWeb(opts.webDir, req, res));

  async function route(req: IncomingMessage, res: ServerResponse, who: Principal | undefined) {
    try {
      if (await ownRoute(req, res, who)) return;
      if (!/^\/sessions\/[^/]+\//.test(req.url ?? '')) return sendProblem(res, 404, 'not found');
      const target = sessionPath(req.url);
      if (!target) return sendProblem(res, 400, 'invalid session key');
      const { key, path, search, agent } = target;
      if (req.method === 'GET' && path === '/approvals') return await peekApprovals(key, search, res);
      if (req.method === 'GET' && path === '/export') return await exportRoute(key, agent, res);
      await withSession(key, agent, `${req.method} ${path}`, async (url) => {
        const body = req.method === 'GET' || req.method === 'HEAD' ? undefined : new Uint8Array(await readBody(req));
        await forward(url + path + search, req.method ?? 'GET', body, res, upstream(key, who));
      });
    } catch (err) {
      sendProblem(res, errorStatus(err), err instanceof Error ? err.message : String(err));
    }
  }

  const server = createServer((req, res) => {
    const who = principalOf(req, opts.proxySecret);
    if (!who) return sendProblem(res, 401, 'identity header without the proxy\'s proof');
    void route(req, res, who);
  });
  if (opts.refuse) server.on('connection', (socket) => void (opts.refuse!(socket.remoteAddress) && socket.destroy()));
  server.on('upgrade', async (req, socket, head) => {
    const target = sessionPath(req.url);
    const who = principalOf(req, opts.proxySecret);
    if (!target || target.path !== '/ws' || !who) return socket.destroy();
    hold(target.key, 1);
    const open = sockets.get(target.key) ?? sockets.set(target.key, new Set()).get(target.key)!;
    open.add(socket);
    socket.once('close', () => { hold(target.key, -1); open.delete(socket); lastUse.set(target.key, now()); });
    try {
      await stopping.get(target.key);
      await removing.get(target.key);
      await mustExist(target.key, 'GET /ws');
      const url = await driver.ensure(target.key, target.agent);
      arm(target.key, undefined);
      await proxyUpgrade(url, upstream(target.key, who), req, '/ws', socket, head);
    } catch {
      socket.destroy();
    }
  });
  const sessions = sessionListener(route, opts.sessions, api);
  server.on('close', () => { sweepStop(); wakeTimers.forEach((cancel) => cancel()); if (sessions.listening) sessions.close(); });
  return Object.assign(server, { drain, sweep: () => maintain(sweepOnce), sessions });
}

/** The only listener a session's environment names: every request needs a current launch credential, identity headers count for nothing, and the credential reaches only what sessionRefusal allows. */
function sessionListener(route: (req: IncomingMessage, res: ServerResponse, who: undefined) => Promise<unknown>, auth: SessionAuth | undefined, api?: (req: IncomingMessage, res: ServerResponse, caller: Caller) => Promise<boolean>) {
  const caller = async (req: IncomingMessage): Promise<SessionCaller | undefined> => {
    const token = sessionToken(req);
    const key = token === undefined ? undefined : auth?.sessionOf(token);
    return key === undefined ? undefined : { key, crossSession: await auth!.crossSession(key) };
  };
  const server = createServer(async (req, res) => {
    try {
      const who = await caller(req);
      if (!who) return sendProblem(res, 401, 'unauthorized');
      // Routed by the normalized path the policy saw, so a `..` cannot reach a route it refused.
      const { pathname, search } = new URL(req.url ?? '', 'http://distributor');
      req.url = pathname + search;
      if (await auth?.events?.(req, res, who.key)) return;
      // The Sessions API holds a session to its own key and the home agent's grant itself.
      if (await api?.(req, res, { kind: 'session', ...who })) return;
      const refused = sessionRefusal(req.method ?? 'GET', pathname, who);
      if (refused) return sendProblem(res, ...refused);
      await route(req, res, undefined);
    } catch (err) {
      sendProblem(res, 502, err instanceof Error ? err.message : String(err));
    }
  });
  return server.on('upgrade', (_req, socket) => socket.destroy());
}
