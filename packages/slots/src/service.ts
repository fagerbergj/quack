import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendProblem, serviceHttp, tooLarge, type App } from '@quack/sdk';
import { SlotTable } from './table.ts';

export type ServiceConfig = {
  /** SQLite file on the service's volume; ':memory:' for tests. */
  dbPath: string;
  /** Slots per endpoint, for example { "llm-swap": 6 }. */
  limits: Record<string, number>;
  leaseTtlMs?: number;
  now?: () => number;
};

const http = serviceHttp({ maxBody: 1 << 16 });
const { send } = http;

export function createSlotsService(config: ServiceConfig): App & { stop(): void } {
  const table = new SlotTable({ dbPath: config.dbPath, limits: config.limits, ttlMs: config.leaseTtlMs ?? 30_000, now: config.now ?? Date.now });
  const handle = async (req: IncomingMessage, res: ServerResponse) => {
    if (req.method === 'GET' && req.url === '/') return send(res, 200, { endpoints: table.snapshot() });
    const action = req.method === 'POST' ? /^\/(acquire|heartbeat|release)$/.exec(req.url ?? '')?.[1] : undefined;
    if (!action) return send(res, 404, { error: 'not found' });
    const body = (await http.json(req).catch((err) => (tooLarge(err) ? err : null))) as { endpoint?: unknown; holder?: unknown } | Error | null;
    if (body instanceof Error) return sendProblem(res, 413, 'body too large');
    if (typeof body?.endpoint !== 'string' || typeof body.holder !== 'string' || !body.holder) return send(res, 400, { error: 'endpoint and holder are required' });
    if (action === 'acquire') return send(res, 200, table.acquire(body.endpoint, body.holder));
    if (action === 'heartbeat') return send(res, 200, { held: table.heartbeat(body.endpoint, body.holder) });
    table.release(body.endpoint, body.holder);
    send(res, 200, {});
  };
  // An unhandled rejection would kill the process, so a disk fault answers 500 instead.
  return { handle: (req, res) => handle(req, res).catch((err) => send(res, 500, { error: (err as Error).message })), stop: () => table.close() };
}
