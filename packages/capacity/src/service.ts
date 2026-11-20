import type { IncomingMessage, ServerResponse } from 'node:http';
import { methodNotAllowed, safeDecode, sendProblem, serviceHttp, type App } from '@quack/sdk';
import { LeaseLedger } from './ledger.ts';

export type ServiceConfig = {
  /** SQLite file on the service's volume; ':memory:' for tests. */
  dbPath: string;
  /** Capacity per endpoint, for example { "local": 6 }. */
  capacity: Record<string, number>;
  leaseTtlMs?: number;
  now?: () => number;
};

const { send } = serviceHttp();

const LEASE = /^\/endpoints\/([^/]+)\/leases\/([^/:]+)(:heartbeat)?$/;
const ENDPOINT = /^\/endpoints\/([^/]+)$/;

class BadPath extends Error {}
const decode = (segment: string) => {
  const decoded = safeDecode(segment);
  if (decoded === undefined) throw new BadPath(`${segment} is not valid percent-encoding`);
  return decoded;
};

export function createCapacityService(config: ServiceConfig): App & { stop(): void } {
  const ledger = new LeaseLedger({ dbPath: config.dbPath, capacity: config.capacity, ttlMs: config.leaseTtlMs ?? 30_000, now: config.now ?? Date.now });

  function lease(method: string, endpoint: string, holder: string, res: ServerResponse) {
    if (method === 'PUT') return send(res, 200, ledger.acquire(endpoint, holder));
    if (method === 'DELETE') return (ledger.release(endpoint, holder), res.writeHead(204).end());
    if (method !== 'GET') return methodNotAllowed(res, 'DELETE, GET, PUT');
    const found = ledger.lease(endpoint, holder);
    return found ? send(res, 200, found) : sendProblem(res, 404, 'no lease for this holder');
  }

  function heartbeat(method: string, endpoint: string, holder: string, res: ServerResponse) {
    if (method !== 'POST') return methodNotAllowed(res, 'POST');
    const found = ledger.heartbeat(endpoint, holder);
    if (!found) return sendProblem(res, 404, 'no lease for this holder; acquire again');
    return found.state === 'queued' ? sendProblem(res, 409, 'the lease is queued, not held') : send(res, 200, found);
  }

  function route(req: IncomingMessage, res: ServerResponse) {
    const path = new URL(req.url ?? '', 'http://x').pathname;
    const method = req.method ?? 'GET';
    const one = LEASE.exec(path);
    if (one) return (one[3] ? heartbeat : lease)(method, decode(one[1]!), decode(one[2]!), res);
    if (path === '/endpoints') return method === 'GET' ? send(res, 200, { endpoints: ledger.endpoints() }) : methodNotAllowed(res, 'GET');
    const endpoint = ENDPOINT.exec(path);
    if (!endpoint) return sendProblem(res, 404, 'not found');
    if (method !== 'GET') return methodNotAllowed(res, 'GET');
    const view = ledger.endpoint(decode(endpoint[1]!));
    return view ? send(res, 200, view) : sendProblem(res, 404, 'no such gated endpoint');
  }

  // An unhandled rejection would kill the process, so a disk fault answers 500 instead.
  const handle = async (req: IncomingMessage, res: ServerResponse) => {
    try {
      route(req, res);
    } catch (err) {
      sendProblem(res, err instanceof BadPath ? 400 : 500, (err as Error).message);
    }
  };
  return { handle, stop: () => ledger.close() };
}
