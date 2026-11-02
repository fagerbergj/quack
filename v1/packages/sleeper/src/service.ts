import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { serviceHttp } from '@quack/sdk';
import type { SleeperClient } from './client.ts';
import type { StartConfig } from './jobs.ts';
import { startLineup } from './lineup/jobs.ts';
import { startFinder, startTrade, tradeParams } from './trades/jobs.ts';
import { startTrends } from './trends/jobs.ts';
import { historyParams, startHistory } from './history/jobs.ts';
import { startDigest, startRetro } from './report/jobs.ts';
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
  /** The league the page's /apps/sleeper routes act on. */
  league?: string;
  /** The schedules service, read for this league's upcoming runs. */
  schedules?: { url: string; token: string };
  fetch?: typeof fetch;
};

const { send, authorized } = serviceHttp();

function safeDecode(s: string): string | undefined {
  try {
    return decodeURIComponent(s);
  } catch {
    return undefined;
  }
}

const PAGE_JOBS = { lineup: startLineup, waivers: startWaivers, trends: startTrends, digest: startDigest, retro: startRetro, 'trade-finder': startFinder, history: startHistory };

export function createSleeperService(config: ServiceConfig) {
  const store = openStore(config.dbPath);
  const now = config.now ?? (() => new Date());

  // GET /leagues/:id/snapshots[/latest|/YYYY-MM-DD] read the store; POST /leagues/:id/snapshots takes today's if it is missing.
  async function route(method: string, url: URL): Promise<[number, unknown]> {
    const path = url.pathname;
    if (path.startsWith('/apps/sleeper')) return pageRoute(method, url);
    const job = /^\/leagues\/([^/]+)\/jobs\/(lineup|waivers|trends|digest|retro)$/.exec(path);
    if (job) return startOnDemand({ lineup: startLineup, waivers: startWaivers, trends: startTrends, digest: startDigest, retro: startRetro }[job[2] as 'lineup'], method, safeDecode(job[1]), url.searchParams.get('week'));
    const trade = /^\/leagues\/([^/]+)\/jobs\/(trade-finder|trade)$/.exec(path);
    if (trade) return startOnDemand(trade[2] === 'trade' ? startTrade : startFinder, method, safeDecode(trade[1]), url.searchParams.get('week'), trade[2] === 'trade' ? tradeParams(url.searchParams) : {});
    const history = /^\/leagues\/([^/]+)\/jobs\/history$/.exec(path);
    if (history) return startOnDemand(startHistory, method, safeDecode(history[1]), url.searchParams.get('week'), historyParams(url.searchParams));
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

  // What the page reaches through the distributor's app proxy: the league id and its jobs, for the configured league.
  async function pageRoute(method: string, url: URL): Promise<[number, unknown]> {
    const job = /^\/apps\/sleeper\/(lineup|waivers|trends|digest|retro|trade-finder|history)$/.exec(url.pathname)?.[1];
    if (job) return startOnDemand(PAGE_JOBS[job as keyof typeof PAGE_JOBS], method, config.league, url.searchParams.get('week'), job === 'history' ? historyParams(url.searchParams) : {});
    if (url.pathname === '/apps/sleeper/schedule' && method === 'GET') return upcomingRuns();
    if (url.pathname !== '/apps/sleeper' || method !== 'GET') return [404, { error: 'not found' }];
    return config.league ? [200, { league: config.league }] : [501, { error: 'no league configured' }];
  }

  // Sleeper's live week and the next run of each of this league's schedules, soonest first.
  async function upcomingRuns(): Promise<[number, unknown]> {
    if (!config.schedules || !config.league) return [501, { error: 'schedules are not configured' }];
    const res = await (config.fetch ?? fetch)(`${config.schedules.url}/schedules`, { headers: { authorization: `Bearer ${config.schedules.token}` } });
    if (!res.ok) return [502, { error: `schedules answered ${res.status}` }];
    const league = config.league.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    const all = ((await res.json()) as { schedules: Array<{ id: string; nextRun: string | null }> }).schedules;
    const runs = all.flatMap((s) => {
      const m = new RegExp(`^sleeper/(lineup|waivers|trends|digest|retro|trade-finder)/${league}(?:/|$)`).exec(s.id);
      return m && s.nextRun ? [{ job: m[1]!, at: s.nextRun }] : [];
    });
    return [200, { week: (await config.client.state()).week, runs: runs.sort((a, b) => a.at.localeCompare(b.at)) }];
  }

  // POST /leagues/:id/jobs/(lineup|waivers|trends|digest|retro)[?week=N] starts a job on demand (SL-2); the week defaults to Sleeper's current one.
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
