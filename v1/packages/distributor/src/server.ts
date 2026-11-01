import { createServer, request, type IncomingMessage, type ServerResponse } from 'node:http';
import { readFile } from 'node:fs/promises';
import { connect } from 'node:net';
import { extname, resolve, sep } from 'node:path';
import type { Duplex } from 'node:stream';
import { AgentError, type HostDriver } from './process-driver.ts';

const KEY = /^[A-Za-z0-9:/#._@-]{1,200}$/;
const MAX_BODY = 1 << 20;

async function readBody(req: IncomingMessage): Promise<Buffer> {
  let size = 0;
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    size += chunk.length;
    if (size > MAX_BODY) throw new Error('body too large');
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}

function safeDecode(s: string): string | undefined {
  try {
    return decodeURIComponent(s);
  } catch {
    return undefined;
  }
}

function send(res: ServerResponse, status: number, body: unknown) {
  res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
}

const TYPES: Record<string, string> = {
  '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.json': 'application/json', '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon', '.map': 'application/json',
};

// Serves a file under root for a GET path; false when the path escapes root or names no file.
async function serveStatic(root: string, pathname: string, res: ServerResponse): Promise<boolean> {
  const rel = safeDecode(pathname);
  if (rel === undefined || rel.includes('\0')) return false;
  const file = resolve(root, '.' + (rel.endsWith('/') ? rel + 'index.html' : rel));
  if (!file.startsWith(resolve(root) + sep)) return false;
  try {
    const data = await readFile(file);
    res.writeHead(200, { 'content-type': TYPES[extname(file)] ?? 'application/octet-stream' }).end(data);
    return true;
  } catch {
    return false;
  }
}

// Static files answer GETs that are not API routes, and only when a web dir is configured.
async function serveWeb(root: string | undefined, req: IncomingMessage, res: ServerResponse): Promise<boolean> {
  if (!root || req.method !== 'GET') return false;
  const { pathname } = new URL(req.url ?? '', 'http://distributor');
  return !/^\/(sessions|agents)(\/|$)/.test(pathname) && serveStatic(root, pathname, res);
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
function forward(url: string, method: string, body: Uint8Array | undefined, res: ServerResponse): Promise<void> {
  return new Promise((resolve, reject) => {
    const up = request(url, { method, headers: { 'content-type': 'application/json' } }, (r) => {
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
export function createDistributor(driver: HostDriver, opts: { idleMs?: number; webDir?: string; index?: (req: IncomingMessage, res: ServerResponse) => Promise<boolean>; now?: () => number; setTimer?: SetTimer } = {}) {
  const now = opts.now ?? Date.now;
  const setTimer = opts.setTimer ?? realTimer;
  const lastUse = new Map<string, number>();
  const stopping = new Map<string, Promise<void>>();
  const holds = new Map<string, number>();
  // Whether each running session was mid-run at its last sweep; a listing reads this and never pings.
  const active = new Map<string, boolean>();
  const notes = new Map<string, string>();
  const wakeTimers = new Map<string, () => void>();
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
    const stop = driver.stop(key).finally(() => stopping.delete(key));
    stopping.set(key, stop);
    await driver.setWake(key, at);
    arm(key, at);
    await stop;
  }

  async function sweepOnce() {
    for (const key of driver.running()) {
      try {
        const { status, wake, waitingOnHuman, note } = await ping(key);
        notes.set(key, note ?? '');
        active.set(key, status === 'HealthyBusy' || Boolean(waitingOnHuman));
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
    await Promise.all(driver.running().map(async (key) => stopSession(key, await ping(key).then(wakeFor, () => undefined))));
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

  const server = createServer(async (req, res) => {
    try {
      if (await listing(req, res)) return;
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
        const url = await driver.ensure(key, agent);
        arm(key, undefined);
        const body = req.method === 'GET' || req.method === 'HEAD' ? undefined : new Uint8Array(await readBody(req));
        await forward(url + path + search, req.method ?? 'GET', body, res);
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
