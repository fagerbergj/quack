import { timingSafeEqual } from 'node:crypto';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { SlotTable } from './table.ts';

export type ServiceConfig = {
  /** shortcut: one shared bearer token authenticates callers until workload tokens (A12) land. */
  serviceToken: string;
  /** SQLite file on the service's volume; ':memory:' for tests. */
  dbPath: string;
  /** Slots per endpoint, for example { "llm-swap": 6 }. */
  limits: Record<string, number>;
  leaseTtlMs?: number;
  now?: () => number;
};

const send = (res: ServerResponse, status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));

function authorized(req: IncomingMessage, token: string): boolean {
  const given = Buffer.from(req.headers.authorization ?? '');
  const expected = Buffer.from(`Bearer ${token}`);
  return given.length === expected.length && timingSafeEqual(given, expected);
}

async function json(req: IncomingMessage): Promise<{ endpoint?: unknown; holder?: unknown } | null> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > 1 << 16) return null;
    chunks.push(chunk);
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString('utf8'));
  } catch {
    return null;
  }
}

export function createSlotsService(config: ServiceConfig) {
  const table = new SlotTable({ dbPath: config.dbPath, limits: config.limits, ttlMs: config.leaseTtlMs ?? 30_000, now: config.now ?? Date.now });
  const server = createServer(async (req, res) => {
    if (!authorized(req, config.serviceToken)) return send(res, 401, { error: 'unauthorized' });
    if (req.method === 'GET' && req.url === '/slots') return send(res, 200, { endpoints: table.snapshot() });
    const action = req.method === 'POST' ? /^\/slots\/(acquire|heartbeat|release)$/.exec(req.url ?? '')?.[1] : undefined;
    if (!action) return send(res, 404, { error: 'not found' });
    const body = await json(req);
    if (typeof body?.endpoint !== 'string' || typeof body.holder !== 'string' || !body.holder) return send(res, 400, { error: 'endpoint and holder are required' });
    if (action === 'acquire') return send(res, 200, table.acquire(body.endpoint, body.holder));
    if (action === 'heartbeat') return send(res, 200, { held: table.heartbeat(body.endpoint, body.holder) });
    table.release(body.endpoint, body.holder);
    send(res, 200, {});
  });
  server.on('close', () => table.close());
  return server;
}
