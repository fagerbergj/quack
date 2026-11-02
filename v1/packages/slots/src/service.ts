import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { serviceHttp } from '@quack/sdk';
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

const http = serviceHttp({ maxBody: 1 << 16 });
const { send, authorized } = http;

export function createSlotsService(config: ServiceConfig) {
  const table = new SlotTable({ dbPath: config.dbPath, limits: config.limits, ttlMs: config.leaseTtlMs ?? 30_000, now: config.now ?? Date.now });
  const handle = async (req: IncomingMessage, res: ServerResponse) => {
    if (!authorized(req, config.serviceToken)) return send(res, 401, { error: 'unauthorized' });
    if (req.method === 'GET' && req.url === '/slots') return send(res, 200, { endpoints: table.snapshot() });
    const action = req.method === 'POST' ? /^\/slots\/(acquire|heartbeat|release)$/.exec(req.url ?? '')?.[1] : undefined;
    if (!action) return send(res, 404, { error: 'not found' });
    const body = (await http.json(req).catch(() => null)) as { endpoint?: unknown; holder?: unknown } | null;
    if (typeof body?.endpoint !== 'string' || typeof body.holder !== 'string' || !body.holder) return send(res, 400, { error: 'endpoint and holder are required' });
    if (action === 'acquire') return send(res, 200, table.acquire(body.endpoint, body.holder));
    if (action === 'heartbeat') return send(res, 200, { held: table.heartbeat(body.endpoint, body.holder) });
    table.release(body.endpoint, body.holder);
    send(res, 200, {});
  };
  // An unhandled rejection would kill the process, so a disk fault answers 500 instead.
  const server = createServer((req, res) => void handle(req, res).catch((err) => send(res, 500, { error: (err as Error).message })));
  server.on('close', () => table.close());
  return server;
}
