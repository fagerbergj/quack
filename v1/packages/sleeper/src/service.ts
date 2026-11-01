import { timingSafeEqual } from 'node:crypto';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import type { SleeperClient } from './client.ts';
import type { StartConfig } from './jobs.ts';
import { startLineup } from './lineup/jobs.ts';
import { startFinder, startTrade, tradeParams } from './trades/jobs.ts';
import { startTrends } from './trends/jobs.ts';
import { startWaivers } from './waivers/jobs.ts';
import { takeSnapshot } from './snapshot.ts';
import { openStore } from './store.ts';

export type ServiceConfig = {
  /** shortcut: one shared bearer token authenticates callers until workload tokens (A12) land. */
  serviceToken: string;
  /** SQLite file on the service's volume; ':memory:' for tests. */
  dbPath: string;
  client: SleeperClient;
  now?: () => Date;
  /** Where on-demand jobs are submitted, and whose team they run for; both are needed to start one. */
  distributorUrl?: string;
  owner?: string;
  fetch?: typeof fetch;
};

const send = (res: ServerResponse, status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));

function authorized(req: IncomingMessage, token: string): boolean {
  const given = Buffer.from(req.headers.authorization ?? '');
  const expected = Buffer.from(`Bearer ${token}`);
  return given.length === expected.length && timingSafeEqual(given, expected);
}

function safeDecode(s: string): string | undefined {
  try {
    return decodeURIComponent(s);
  } catch {
    return undefined;
  }
}

export function createSleeperService(config: ServiceConfig) {
  const store = openStore(config.dbPath);
  const now = config.now ?? (() => new Date());

  // GET /leagues/:id/snapshots[/latest|/YYYY-MM-DD] read the store; POST /leagues/:id/snapshots takes today's if it is missing.
  async function route(method: string, url: URL): Promise<[number, unknown]> {
    const path = url.pathname;
    const job = /^\/leagues\/([^/]+)\/jobs\/(lineup|waivers|trends)$/.exec(path);
    if (job) return startOnDemand({ lineup: startLineup, waivers: startWaivers, trends: startTrends }[job[2] as 'lineup'], method, safeDecode(job[1]), url.searchParams.get('week'));
    const trade = /^\/leagues\/([^/]+)\/jobs\/(trade-finder|trade)$/.exec(path);
    if (trade) return startOnDemand(trade[2] === 'trade' ? startTrade : startFinder, method, safeDecode(trade[1]), url.searchParams.get('week'), trade[2] === 'trade' ? tradeParams(url.searchParams) : {});
    const m = /^\/leagues\/([^/]+)\/snapshots(\/latest|\/\d{4}-\d{2}-\d{2})?$/.exec(path);
    if (!m) return [404, { error: 'not found' }];
    const league = safeDecode(m[1]);
    if (league === undefined) return [400, { error: 'bad league id' }];
    if (method === 'POST' && !m[2]) return [200, await takeSnapshot(config.client, store, league, now())];
    if (method !== 'GET') return [405, { error: 'method not allowed' }];
    if (!m[2]) return [200, { snapshots: store.history(league) }];
    const found = m[2] === '/latest' ? store.latest(league) : store.snapshot(league, m[2].slice(1));
    return found ? [200, found] : [404, { error: 'no snapshot yet' }];
  }

  // POST /leagues/:id/jobs/(lineup|waivers|trends)[?week=N] starts a job on demand (SL-2); the week defaults to Sleeper's current one.
  async function startOnDemand(start: (c: StartConfig, input: any) => ReturnType<typeof startLineup>, method: string, league: string | undefined, week: string | null, extra: object | string = {}): Promise<[number, unknown]> {
    if (league === undefined) return [400, { error: 'bad league id' }];
    if (method !== 'POST') return [405, { error: 'method not allowed' }];
    if (!config.distributorUrl || !config.owner) return [501, { error: 'on-demand jobs are not configured' }];
    const n = week === null ? (await config.client.state()).week : Number(week);
    if (!Number.isInteger(n) || n < 1) return [400, { error: 'bad week' }];
    if (typeof extra === 'string') return [400, { error: extra }];
    const r = await start({ distributorUrl: config.distributorUrl, fetch: config.fetch, now }, { league, week: n, owner: config.owner, ...extra });
    return [r.ok ? 202 : 502, { sessionKey: r.sessionKey, status: r.status }];
  }

  const server = createServer(async (req, res) => {
    try {
      if (!authorized(req, config.serviceToken)) return send(res, 401, { error: 'unauthorized' });
      const [status, body] = await route(req.method ?? 'GET', new URL(req.url ?? '', 'http://x'));
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
