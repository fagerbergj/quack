import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import type { HostDriver } from './process-driver.ts';

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

function send(res: ServerResponse, status: number, body: unknown) {
  res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
}

// Routes /sessions/<url-encoded key>/<path> to the one process that owns that session (DSP-1..4). Holds no domain logic.
export function createDistributor(driver: HostDriver, opts: { idleMs?: number } = {}) {
  const lastUse = new Map<string, number>();
  const idleMs = opts.idleMs ?? 10 * 60_000;

  const sweep = setInterval(async () => {
    for (const key of driver.running()) {
      if (Date.now() - (lastUse.get(key) ?? 0) < idleMs) continue;
      try {
        const url = await driver.ensure(key);
        const { status } = (await (await fetch(`${url}/ping`)).json()) as { status: string };
        if (status === 'Healthy') await driver.stop(key);
      } catch {
        // a session that cannot answer /ping is left for the next sweep
      }
    }
  }, Math.max(50, Math.min(idleMs, 30_000)));
  sweep.unref();

  const server = createServer(async (req, res) => {
    try {
      if (req.method === 'GET' && req.url === '/sessions') return send(res, 200, { running: driver.running() });
      const match = /^\/sessions\/([^/]+)(\/.*)$/.exec(req.url ?? '');
      if (!match) return send(res, 404, { error: 'not found' });
      const key = decodeURIComponent(match[1]);
      if (!KEY.test(key)) return send(res, 400, { error: 'invalid session key' });
      lastUse.set(key, Date.now());
      const url = await driver.ensure(key);
      const body = req.method === 'GET' || req.method === 'HEAD' ? undefined : new Uint8Array(await readBody(req));
      const upstream = await fetch(url + match[2], { method: req.method, body, headers: { 'content-type': 'application/json' } });
      lastUse.set(key, Date.now());
      res.writeHead(upstream.status, { 'content-type': upstream.headers.get('content-type') ?? 'application/json' });
      res.end(Buffer.from(await upstream.arrayBuffer()));
    } catch (err) {
      const bad = err instanceof Error && err.message === 'body too large';
      send(res, bad ? 400 : 502, { error: err instanceof Error ? err.message : String(err) });
    }
  });
  server.on('close', () => clearInterval(sweep));
  return server;
}
