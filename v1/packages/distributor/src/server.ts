import { createServer, request, type IncomingMessage, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { connect } from 'node:net';
import { extname, resolve, sep } from 'node:path';
import type { Duplex } from 'node:stream';
import { authorized } from '@quack/index';
import { serviceHttp } from '@quack/sdk';
import { AgentError, type HostDriver } from './process-driver.ts';

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
  return !/^\/(sessions|agents)(\/|$)/.test(pathname) && serveStatic(root, pathname, req, res);
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

type Ping = { status: string; waitingOnHuman?: boolean; wake?: { at?: number }; note?: string | null };

// `now` and `setTimer` are injectable so tests run the idle sweep and wake timers on a fake clock.
export function createDistributor(driver: HostDriver, opts: { idleMs?: number; /** Enables the page's rename, archive and delete routes, and DELETE /sessions/:key for callers bearing `token` when one is set; `waiting` says whether a person's approval or decision is pending and `forget` drops the session's index rows. */ retire?: { token?: string; waiting(key: string): Promise<boolean>; forget(key: string): Promise<void> }; webDir?: string; index?: (req: IncomingMessage, res: ServerResponse) => Promise<boolean>; now?: () => number; setTimer?: SetTimer } = {}) {
  const now = opts.now ?? Date.now;
  const setTimer = opts.setTimer ?? realTimer;
  const lastUse = new Map<string, number>();
  const stopping = new Map<string, Promise<void>>();
  const holds = new Map<string, number>();
  // A delete in flight per key; requests for that key wait it out, then start a fresh session.
  const removing = new Map<string, Promise<void>>();
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
    if (!driver.running().includes(key)) return send(res, 200, { approvals: [] });
    const upstream = await fetch(`${await driver.ensure(key)}/approvals${search}`);
    res.writeHead(upstream.status, { 'content-type': upstream.headers.get('content-type') ?? 'application/json' });
    res.end(Buffer.from(await upstream.arrayBuffer()));
  }

  async function listing(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    if (req.method !== 'GET' || (req.url !== '/sessions' && req.url !== '/agents')) return false;
    send(res, 200, req.url === '/agents' ? { agents: await driver.agents() } : { sessions: (await driver.sessions()).map((x) => ({ ...x, active: x.running && (active.get(x.key) ?? false), ...(x.running && notes.get(x.key) && { note: notes.get(x.key) }) })) });
    return true;
  }

  // One agent's definition at a version, and promotion of its current draft; same boundary as housekeeping.
  async function agentRoutes(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const match = /^\/agents\/([^/?]+)(\/promote|\?version=(\d+))?$/.exec(req.url ?? '');
    const name = match && safeDecode(match[1]!);
    if (!name || (req.method === 'GET' ? match[2]?.startsWith('/') : match[2] !== '/promote')) return false;
    if (req.method === 'GET') {
      const found = await driver.agent(name, match[3] ? Number(match[3]) : undefined);
      return send(res, found ? 200 : 404, found ?? { error: 'no such agent version' }), true;
    }
    if (!req.headers['content-type']?.startsWith('application/json')) return send(res, 415, { error: 'send JSON' }), true;
    const { version } = ((await json(req).catch(() => null)) ?? {}) as { version?: unknown };
    if (typeof version !== 'number') return send(res, 400, { error: 'version must be a number' }), true;
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

  // Marked in `removing` before the first await, so a request either holds the key already (refused here) or waits for the delete.
  async function retireChecked(key: string, res: ServerResponse) {
    const why = driver.running().includes(key) || holds.get(key) || stopping.has(key) ? 'is running'
      : (await driver.wakes()).some((w) => w.key === key) ? 'is sleeping until its wake time'
        : (await opts.retire!.waiting(key)) ? 'is waiting on a person' : undefined;
    if (why) return send(res, 409, { error: `session ${why}` });
    if (!(await driver.remove(key))) return send(res, 404, { error: 'no such session' });
    await opts.retire!.forget(key);
    active.delete(key);
    notes.delete(key);
    send(res, 200, { key });
  }

  async function deletion(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const match = req.method === 'DELETE' && opts.retire?.token ? /^\/sessions\/([^/?]+)$/.exec(req.url ?? '') : null;
    if (!match) return false;
    const key = safeDecode(match[1]);
    if (!authorized(req, opts.retire!.token!)) send(res, 401, { error: 'unauthorized' });
    else if (!key || !KEY.test(key)) send(res, 400, { error: 'invalid session key' });
    else await retire(key, res);
    return true;
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
    if (typeof change === 'string') return send(res, 400, { error: change });
    const { title, archived } = change;
    if (archived && (active.get(key) || (await opts.retire!.waiting(key)))) return send(res, 409, { error: "Can't archive a session that is running or waiting on you." });
    const done = [title === undefined || (await driver.setTitle(key, title.trim() || undefined)), archived === undefined || (await driver.setArchived(key, archived))];
    if (done.includes(false)) return send(res, 404, { error: 'no such session' });
    send(res, 200, { key });
  }

  // The page's routes carry no token: the proxy's identity header (else the tailnet) is the boundary, as for approvals.
  // A JSON content type keeps a cross-site form from posting here.
  async function housekeeping(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const match = opts.retire && /^\/sessions\/([^/?]+)(\/delete)?$/.exec(req.url ?? '');
    if (!match || req.method !== (match[2] ? 'POST' : 'PATCH')) return false;
    const key = safeDecode(match[1]);
    if (!key || !KEY.test(key)) send(res, 400, { error: 'invalid session key' });
    else if (!req.headers['content-type']?.startsWith('application/json')) send(res, 415, { error: 'send JSON' });
    else if (match[2]) {
      console.log(`session ${key} deleted by ${req.headers['x-authentik-username'] ?? 'tailnet'}`);
      await retire(key, res);
    } else await patch(key, req, res);
    return true;
  }

  const removal = async (req: IncomingMessage, res: ServerResponse) => (await deletion(req, res)) || (await housekeeping(req, res));

  const server = createServer(async (req, res) => {
    try {
      if (await listing(req, res) || await agentRoutes(req, res) || await removal(req, res)) return;
      if (await opts.index?.(req, res)) return;
      if (await serveWeb(opts.webDir, req, res)) return;
      if (!/^\/sessions\/[^/]+\//.test(req.url ?? '')) return send(res, 404, { error: 'not found' });
      const target = sessionPath(req.url);
      if (!target) return send(res, 400, { error: 'invalid session key' });
      const { key, path, search, agent } = target;
      if (req.method === 'GET' && path === '/approvals') return await peekApprovals(key, search, res);
      lastUse.set(key, now());
      // A request in flight keeps its session from idling out, however long it waits on the run.
      hold(key, 1);
      try {
        await stopping.get(key);
        await removing.get(key);
        const url = await driver.ensure(key, agent);
        arm(key, undefined);
        const body = req.method === 'GET' || req.method === 'HEAD' ? undefined : new Uint8Array(await readBody(req));
        await forward(url + path + search, req.method ?? 'GET', body, res, req.headers['x-authentik-username']?.toString());
      } finally {
        hold(key, -1);
        lastUse.set(key, now());
      }
    } catch (err) {
      send(res, errorStatus(err), { error: err instanceof Error ? err.message : String(err) });
    }
  });
  server.on('upgrade', async (req, socket, head) => {
    const target = sessionPath(req.url);
    if (!target || target.path !== '/ws') return socket.destroy();
    hold(target.key, 1);
    socket.once('close', () => { hold(target.key, -1); lastUse.set(target.key, now()); });
    try {
      await stopping.get(target.key);
      await removing.get(target.key);
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
