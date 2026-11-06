import { createServer, request, type IncomingMessage, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { connect } from 'node:net';
import { extname, resolve, sep } from 'node:path';
import type { Duplex } from 'node:stream';
import { authorized, type Fact } from '@quack/index';
import { sendProblem, serviceHttp } from '@quack/sdk';
import { AgentError, type HostDriver, type SessionInfo } from './process-driver.ts';
import { listQuery, paged } from './session-list.ts';

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
async function proxyUpgrade(target: string, req: IncomingMessage, path: string, client: Duplex, head: Buffer) {
  const { hostname, port } = new URL(target);
  const upstream = connect(Number(port), hostname);
  await new Promise((resolve, reject) => upstream.once('connect', resolve).once('error', reject));
  const headers = req.rawHeaders.reduce((acc, v, i) => acc + (i % 2 ? `${v}\r\n` : `${v}: `), '');
  upstream.write(`GET ${path} HTTP/1.1\r\n${headers}\r\n`);
  if (head.length) upstream.write(head);
  upstream.pipe(client).pipe(upstream);
  const end = () => { upstream.destroy(); client.destroy(); };
  upstream.on('error', end).on('close', end);
  client.on('error', end).on('close', end);
}

// node:http, not fetch: undici gives up on headers after 300 s, and a wait:true invocation sends none until its run settles.
// A client that hangs up aborts the upstream request.
function forward(url: string, method: string, body: Uint8Array | undefined, res: ServerResponse, user?: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const up = request(url, { method, headers: { 'content-type': 'application/json', ...(user ? { 'x-authentik-username': user } : {}) } }, (r) => {
      res.writeHead(r.statusCode ?? 502, { 'content-type': r.headers['content-type'] ?? 'application/json' });
      r.pipe(res).on('finish', resolve);
      r.on('error', (err) => (res.destroyed || res.writableFinished ? resolve() : reject(err)));
    });
    res.on('close', () => { if (!res.writableFinished) up.destroy(); });
    up.on('error', (err) => (res.headersSent || res.destroyed ? resolve() : reject(err)));
    up.end(body);
  });
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

type Spend = { key: string; cost: number; tokens: { input: number; output: number; cacheRead: number; cacheWrite: number } };

/** A session's `status` fact turning idle is a completed turn, dated when it ended; sessions resend unacknowledged facts, so a run that ended while this process was down still lands. */
export const recordTurns = (driver: Pick<HostDriver, 'touch'>) => async (session: string, facts: Fact[]): Promise<void> => {
  for (const f of facts) if (f.kind === 'status' && f.data.state === 'idle') await driver.touch(session, f.at).catch(() => {});
};

type Ping = { status: string; waitingOnHuman?: boolean; wake?: { at?: number }; note?: string | null };

// `now` and `setTimer` are injectable so tests run the idle sweep and wake timers on a fake clock.
export function createDistributor(driver: HostDriver, opts: { idleMs?: number; /** Enables rename, archive and delete (PATCH, DELETE /sessions/:key and the legacy POST .../delete); each needs `token` as a bearer or the proxy's identity header (the legacy route only the identity); `waiting` says whether a person's approval or decision is pending and `forget` drops the session's index rows. */ retire?: { token?: string; waiting(key: string): Promise<boolean>; forget(key: string): Promise<void> }; webDir?: string; /** Spend per session from the session index, merged into session rows; a rejection means the index is off and rows omit it. */ spend?: () => Promise<Spend[]>; index?: (req: IncomingMessage, res: ServerResponse) => Promise<boolean>; now?: () => number; setTimer?: SetTimer } = {}) {
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
  let draining = false;
  const idleMs = opts.idleMs ?? 10 * 60_000;
  const idle = (key: string) => !holds.get(key) && now() - (lastUse.get(key) ?? 0) >= idleMs;
  const hold = (key: string, by: number) => holds.set(key, (holds.get(key) ?? 0) + by);

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

  async function wake(key: string) {
    try {
      await stopping.get(key);
      await driver.ensure(key);
      // A grace period, so the next sweep does not stop the session before it has run.
      lastUse.set(key, now());
      await driver.setWake(key, undefined);
    } catch {
      // the next request starts the session
    }
  }

  async function ping(key: string): Promise<Ping> {
    return (await (await fetch(`${await driver.ensure(key)}/ping`)).json()) as Ping;
  }

  // Records the session's earliest sleep before stopping, so a restarted distributor still wakes it.
  async function stopSession(key: string, at: number | undefined) {
    active.delete(key);
    notes.delete(key);
    marked.delete(key);
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
    } else if (!busy && marked.delete(key)) await driver.setWake(key, sleepAt);
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
    const upstream = await fetch(`${await driver.ensure(key)}/approvals${search}`);
    res.writeHead(upstream.status, { 'content-type': upstream.headers.get('content-type') ?? 'application/json' });
    res.end(Buffer.from(await upstream.arrayBuffer()));
  }

  // One self-contained file: the session's metadata beside its transcript and sealed artifacts, as a download.
  async function exportSession(info: SessionInfo, url: string, res: ServerResponse) {
    const upstream = await fetch(`${url}/export`);
    if (!upstream.ok) return sendProblem(res, 502, 'session export failed');
    const { running: _running, ...session } = info;
    const name = `quack-${info.key.replace(/[^A-Za-z0-9._-]+/g, '-')}.json`;
    res.writeHead(200, { 'content-type': 'application/json', 'content-disposition': `attachment; filename="${name}"` });
    res.end(JSON.stringify({ session, ...(await upstream.json() as object) }, null, 2));
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

  async function exportRoute(key: string, agent: string | undefined, res: ServerResponse) {
    const info = (await driver.sessions()).find((x) => x.key === key);
    if (!info) return sendProblem(res, 404, 'no such session');
    await withSession(key, agent, 'GET /export', async (url) => void (await exportSession(info, url, res)));
  }

  // Stored values plus what the last sweep saw and the index's spend; the index being off only drops cost and tokens.
  async function decorate(found: SessionInfo[]) {
    const spent = new Map((found.length ? await opts.spend?.().catch(() => []) ?? [] : []).map((x) => [x.key, x]));
    return found.map((x) => {
      const s = spent.get(x.key);
      return { ...x, active: x.running && (active.get(x.key) ?? false), ...(x.running && notes.get(x.key) && { note: notes.get(x.key) }), ...(s && { cost: s.cost, tokens: s.tokens }) };
    });
  }

  async function sessionList(params: URLSearchParams, res: ServerResponse) {
    const query = listQuery(params);
    if (typeof query === 'string') return sendProblem(res, 400, query);
    const { rows, next } = paged((await driver.sessions()).filter(query.keep), query);
    send(res, 200, { sessions: await decorate(rows), ...(next && { nextPageToken: next }) });
  }

  async function sessionOne(key: string | undefined, res: ServerResponse) {
    if (!key || !KEY.test(key)) return sendProblem(res, 400, 'invalid session key');
    const found = (await driver.sessions()).find((x) => x.key === key);
    if (found) send(res, 200, (await decorate([found]))[0]);
    else sendProblem(res, 404, 'no such session');
  }

  async function listing(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const { pathname, searchParams } = new URL(req.url ?? '', 'http://distributor');
    const one = /^\/sessions\/([^/]+)$/.exec(pathname);
    if (req.method !== 'GET' || !(one || ['/sessions', '/agents', '/version'].includes(pathname))) return false;
    if (pathname === '/version') send(res, 200, { commit: process.env.QUACK_COMMIT || null, builtAt: process.env.QUACK_BUILT_AT || null });
    else if (pathname === '/agents') send(res, 200, { agents: await driver.agents() });
    else if (one) await sessionOne(safeDecode(one[1]!), res);
    else await sessionList(searchParams, res);
    return true;
  }

  // One agent's definition at a version, and promotion of its current draft; same boundary as housekeeping.
  async function agentRoutes(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
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
    console.log(`agent ${name} version ${version} promoted by ${req.headers['x-authentik-username'] ?? 'tailnet'}`);
    send(res, 200, { name, version, status });
    return true;
  }

  // Mechanism only: which sessions are worth deleting is the caller's policy.
  async function retire(key: string, res: ServerResponse) {
    let done = () => {};
    removing.set(key, new Promise<void>((r) => (done = r)));
    try {
      await retireChecked(key, res);
    } finally {
      removing.delete(key);
      done();
    }
  }

  // Only work refuses a delete: a run in flight, a sleep with a wake time, a person's pending decision. An idle process, or a page watching it, is stopped first.
  // Marked in `removing` before the first await, so new requests wait for the delete; ones already proxied end when the process stops.
  async function retireChecked(key: string, res: ServerResponse) {
    await stopping.get(key);
    const live = driver.running().includes(key);
    const busy = live && (await ping(key).then((p) => p.status === 'HealthyBusy', () => true));
    const why = busy ? 'is running'
      : serving.has(key) ? 'is handling a request'
      : !marked.has(key) && (await driver.wakes()).some((w) => w.key === key) ? 'is sleeping until its wake time'
        : (await opts.retire!.waiting(key)) ? 'is waiting on a person' : undefined;
    if (why) return sendProblem(res, 409, `session ${why}`);
    for (const s of sockets.get(key) ?? []) s.destroy();
    if (live) await driver.stop(key);
    marked.delete(key);
    if (!(await driver.remove(key))) return sendProblem(res, 404, 'no such session');
    arm(key, undefined);
    await opts.retire!.forget(key);
    active.delete(key);
    notes.delete(key);
    send(res, 200, { key });
  }

  const TITLE_MAX = 120;

  // A title is text of bounded length and archived a boolean; the string is an error for the caller.
  function parsePatch(body: unknown): { title?: string; archived?: boolean } | string {
    const { title, archived } = (body ?? {}) as Record<string, unknown>;
    if (title !== undefined && (typeof title !== 'string' || title.length > TITLE_MAX)) return `title must be text of at most ${TITLE_MAX} characters`;
    if (archived !== undefined && typeof archived !== 'boolean') return 'archived must be true or false';
    return title === undefined && archived === undefined ? 'nothing to change' : { title, archived };
  }

  // Archiving hides a session from the lists, so one mid-run or waiting on a person is refused.
  async function patch(key: string, req: IncomingMessage, res: ServerResponse) {
    const change = parsePatch(await json(req).catch(() => null));
    if (typeof change === 'string') return sendProblem(res, 400, change);
    const { title, archived } = change;
    if (archived && (active.get(key) || (await opts.retire!.waiting(key)))) return sendProblem(res, 409, "Can't archive a session that is running or waiting on you.");
    const done = [title === undefined || (await driver.setTitle(key, title.trim() || undefined)), archived === undefined || (await driver.setArchived(key, archived))];
    if (done.includes(false)) return sendProblem(res, 404, 'no such session');
    const [row] = await decorate((await driver.sessions()).filter((x) => x.key === key));
    return row ? send(res, 200, row) : sendProblem(res, 404, 'no such session');
  }

  // Why a housekeeping request is refused before it runs, if it is: it needs the admin token or the proxy's identity (the legacy route only the identity), and a body must be JSON (a cross-site form cannot send it).
  function refusal(req: IncomingMessage, key: string | undefined, legacy: boolean): [number, string] | undefined {
    const proxied = Boolean(req.headers['x-authentik-username']);
    if (legacy || !req.headers.authorization ? !proxied : !authorized(req, opts.retire!.token ?? '')) return [401, 'unauthorized'];
    if (!key || !KEY.test(key)) return [400, 'invalid session key'];
    return req.method !== 'DELETE' && !req.headers['content-type']?.startsWith('application/json') ? [legacy ? 400 : 415, 'send JSON'] : undefined;
  }

  // Rename, archive and delete change what a person sees, so each needs a credential; reads stay perimeter-trusted.
  async function housekeeping(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const match = opts.retire && /^\/sessions\/([^/]+)(\/delete)?$/.exec(new URL(req.url ?? '', 'http://distributor').pathname);
    const legacy = Boolean(match?.[2]);
    if (!match || !(legacy ? req.method === 'POST' : req.method === 'PATCH' || req.method === 'DELETE')) return false;
    const key = safeDecode(match[1]!);
    const refused = refusal(req, key, legacy);
    if (refused) sendProblem(res, ...refused);
    else if (req.method === 'PATCH') await patch(key!, req, res);
    else {
      console.log(`session ${key} deleted by ${req.headers['x-authentik-username'] ?? 'the admin token'}`);
      await retire(key!, res);
    }
    return true;
  }

  // A submission naming another agent changes the session's role if no run is active (D33); an open run, a pending approval or a sleep is still a run.
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

  const removal = async (req: IncomingMessage, res: ServerResponse) => (await housekeeping(req, res)) || (await agentRoutes(req, res));

  const server = createServer(async (req, res) => {
    try {
      if (await listing(req, res) || await removal(req, res)) return;
      if (await opts.index?.(req, res)) return;
      if (await serveWeb(opts.webDir, req, res)) return;
      if (!/^\/sessions\/[^/]+\//.test(req.url ?? '')) return sendProblem(res, 404, 'not found');
      const target = sessionPath(req.url);
      if (!target) return sendProblem(res, 400, 'invalid session key');
      const { key, path, search, agent } = target;
      if (req.method === 'GET' && path === '/approvals') return await peekApprovals(key, search, res);
      if (req.method === 'GET' && path === '/export') return await exportRoute(key, agent, res);
      await withSession(key, agent, `${req.method} ${path}`, async (url) => {
        const body = req.method === 'GET' || req.method === 'HEAD' ? undefined : new Uint8Array(await readBody(req));
        await forward(url + path + search, req.method ?? 'GET', body, res, req.headers['x-authentik-username']?.toString());
      });
    } catch (err) {
      sendProblem(res, errorStatus(err), err instanceof Error ? err.message : String(err));
    }
  });
  server.on('upgrade', async (req, socket, head) => {
    const target = sessionPath(req.url);
    if (!target || target.path !== '/ws') return socket.destroy();
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
      await proxyUpgrade(url, req, '/ws', socket, head);
    } catch {
      socket.destroy();
    }
  });
  server.on('close', () => { sweepStop(); wakeTimers.forEach((cancel) => cancel()); });
  return Object.assign(server, { drain, sweep: () => maintain(sweepOnce) });
}
