import { timingSafeEqual } from 'node:crypto';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import type { SleeperClient } from './client.ts';
import { takeSnapshot } from './snapshot.ts';
import { openStore } from './store.ts';

export type ServiceConfig = {
  /** shortcut: one shared bearer token authenticates callers until workload tokens (A12) land. */
  serviceToken: string;
  /** SQLite file on the service's volume; ':memory:' for tests. */
  dbPath: string;
  client: SleeperClient;
  now?: () => Date;
};

const send = (res: ServerResponse, status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));

function authorized(req: IncomingMessage, token: string): boolean {
  const given = Buffer.from(req.headers.authorization ?? '');
  const expected = Buffer.from(`Bearer ${token}`);
  return given.length === expected.length && timingSafeEqual(given, expected);
}

export function createSleeperService(config: ServiceConfig) {
  const store = openStore(config.dbPath);
  const now = config.now ?? (() => new Date());

  // GET /leagues/:id/snapshots[/latest] read the store; POST /leagues/:id/snapshots takes today's if it is missing.
  async function route(method: string, path: string): Promise<[number, unknown]> {
    const m = /^\/leagues\/([^/]+)\/snapshots(\/latest)?$/.exec(path);
    if (!m) return [404, { error: 'not found' }];
    const league = decodeURIComponent(m[1]);
    if (method === 'POST' && !m[2]) return [200, await takeSnapshot(config.client, store, league, now())];
    if (method !== 'GET') return [405, { error: 'method not allowed' }];
    if (!m[2]) return [200, { snapshots: store.history(league) }];
    const latest = store.latest(league);
    return latest ? [200, latest] : [404, { error: 'no snapshot yet' }];
  }

  const server = createServer(async (req, res) => {
    try {
      if (!authorized(req, config.serviceToken)) return send(res, 401, { error: 'unauthorized' });
      const [status, body] = await route(req.method ?? 'GET', new URL(req.url ?? '', 'http://x').pathname);
      send(res, status, body);
    } catch (err) {
      send(res, 502, { error: (err as Error).message });
    }
  });
  return Object.assign(server, {
    stop() {
      server.close();
      store.close();
    },
  });
}
