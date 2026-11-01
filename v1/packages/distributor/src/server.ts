import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { connect } from 'node:net';
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

// Routes /sessions/<url-encoded key>/<path> to the one process that owns that session (DSP-1..4). Holds no domain logic.
function sessionPath(url: string | undefined): { key: string; path: string; agent?: string } | undefined {
  const { pathname, searchParams } = new URL(url ?? '', 'http://distributor');
  const match = /^\/sessions\/([^/]+)(\/.*)$/.exec(pathname);
  if (!match) return undefined;
  const key = safeDecode(match[1]);
  return key && KEY.test(key) ? { key, path: match[2], agent: searchParams.get('agent') || undefined } : undefined;
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

function errorStatus(err: unknown): number {
  if (err instanceof AgentError) return err.status;
  return err instanceof Error && err.message === 'body too large' ? 400 : 502;
}

export function createDistributor(driver: HostDriver, opts: { idleMs?: number } = {}) {
  const lastUse = new Map<string, number>();
  const stopping = new Map<string, Promise<void>>();
  const viewers = new Map<string, number>();
  const idleMs = opts.idleMs ?? 10 * 60_000;
  const idle = (key: string) => !viewers.get(key) && Date.now() - (lastUse.get(key) ?? 0) >= idleMs;

  const sweep = setInterval(async () => {
    for (const key of driver.running()) {
      if (!idle(key)) continue;
      try {
        const url = await driver.ensure(key);
        const { status } = (await (await fetch(`${url}/ping`)).json()) as { status: string };
        // Re-checked after the ping, and requests wait out a stop in flight, so none reaches a dying process.
        if (status !== 'Healthy' || !idle(key)) continue;
        const stop = driver.stop(key).finally(() => stopping.delete(key));
        stopping.set(key, stop);
        await stop;
      } catch {
        // a session that cannot answer /ping is left for the next sweep
      }
    }
  }, Math.max(50, Math.min(idleMs, 30_000)));
  sweep.unref();

  const server = createServer(async (req, res) => {
    try {
      if (req.method === 'GET' && req.url === '/sessions') return send(res, 200, { sessions: await driver.sessions() });
      if (req.method === 'GET' && req.url === '/agents') return send(res, 200, { agents: await driver.agents() });
      if (!/^\/sessions\/[^/]+\//.test(req.url ?? '')) return send(res, 404, { error: 'not found' });
      const target = sessionPath(req.url);
      if (!target) return send(res, 400, { error: 'invalid session key' });
      const { key, path, agent } = target;
      lastUse.set(key, Date.now());
      await stopping.get(key);
      const url = await driver.ensure(key, agent);
      const body = req.method === 'GET' || req.method === 'HEAD' ? undefined : new Uint8Array(await readBody(req));
      const upstream = await fetch(url + path, { method: req.method, body, headers: { 'content-type': 'application/json' } });
      lastUse.set(key, Date.now());
      res.writeHead(upstream.status, { 'content-type': upstream.headers.get('content-type') ?? 'application/json' });
      res.end(Buffer.from(await upstream.arrayBuffer()));
    } catch (err) {
      send(res, errorStatus(err), { error: err instanceof Error ? err.message : String(err) });
    }
  });
  server.on('upgrade', async (req, socket, head) => {
    const target = sessionPath(req.url);
    if (!target || target.path !== '/ws') return socket.destroy();
    viewers.set(target.key, (viewers.get(target.key) ?? 0) + 1);
    socket.once('close', () => { viewers.set(target.key, (viewers.get(target.key) ?? 1) - 1); lastUse.set(target.key, Date.now()); });
    try {
      await stopping.get(target.key);
      await proxyUpgrade(await driver.ensure(target.key, target.agent), req, '/ws', socket, head);
    } catch {
      socket.destroy();
    }
  });
  server.on('close', () => clearInterval(sweep));
  return server;
}
