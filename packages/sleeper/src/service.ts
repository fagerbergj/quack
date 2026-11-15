import { sendProblem, serviceHttp, type App } from '@quack/sdk';
import type { SleeperClient } from './client.ts';
import type { StartConfig } from './jobs.ts';
import { startLineup } from './lineup/jobs.ts';
import { startFinder } from './trades/jobs.ts';
import { startTrends } from './trends/jobs.ts';
import { startHistory } from './history/jobs.ts';
import { startDigest, startRetro } from './report/jobs.ts';
import { startWaivers } from './waivers/jobs.ts';
import { takeSnapshot } from './snapshot.ts';
import { openStore } from './store.ts';

export type ServiceConfig = {
  /** SQLite file on the service's volume; ':memory:' for tests. */
  dbPath: string;
  client: SleeperClient;
  now?: () => Date;
  /** Where on-demand jobs are submitted, and whose team they run for; both are needed to start one. */
  distributorUrl?: string;
  owner?: string;
  /** The league the page's /apps/sleeper routes act on. */
  league?: string;
  /** The schedules app (a base URL and the fetch that reaches it), read for this league's upcoming runs. */
  schedules?: { url: string; fetch?: typeof fetch };
  fetch?: typeof fetch;
};

const { send } = serviceHttp();

/** A reply the route chose to refuse with; `allow` is a 405's Allow header. */
class HttpError extends Error {
  status: number;
  allow?: string;
  constructor(status: number, detail: string, allow?: string) {
    super(detail);
    this.status = status;
    this.allow = allow;
  }
}

function safeDecode(s: string): string {
  try {
    return decodeURIComponent(s);
  } catch {
    throw new HttpError(400, 'bad league id');
  }
}

const allow = (method: string, ...ok: string[]) => {
  if (!ok.includes(method)) throw new HttpError(405, 'method not allowed', ok.join(', '));
};

const PAGE_JOBS = { lineup: startLineup, waivers: startWaivers, trends: startTrends, digest: startDigest, retro: startRetro, 'trade-finder': startFinder, history: startHistory };
const MAX_PAGE_SIZE = 1000;
const DATE = /^\d{4}-\d{2}-\d{2}$/;

// The token is the last date served, base64url; only one that re-encodes to itself decodes.
const pageToken = (date: string) => Buffer.from(date).toString('base64url');
function afterOf(token: string | null): string | undefined {
  if (!token) return undefined;
  const date = Buffer.from(token, 'base64url').toString();
  if (DATE.test(date) && pageToken(date) === token) return date;
  throw new HttpError(400, 'pageToken is not one this service issued');
}

function pageSizeOf(size: string | null): number {
  const n = size === null ? 100 : /^\d+$/.test(size) ? Number(size) : 0;
  if (n < 1 || n > MAX_PAGE_SIZE) throw new HttpError(400, `pageSize must be an integer from 1 to ${MAX_PAGE_SIZE}`);
  return n;
}

export function createSleeperService(config: ServiceConfig): App & { stop(): void } {
  const store = openStore(config.dbPath);
  const now = config.now ?? (() => new Date());

  // Answers [status, body]; a refusal throws an HttpError. A wrong method on a known path is a 405, an unknown path a 404.
  async function route(method: string, url: URL): Promise<[number, unknown]> {
    const path = url.pathname;
    if (path === '/') {
      allow(method, 'GET');
      if (!config.league) throw new HttpError(501, 'no league configured');
      return [200, { league: config.league }];
    }
    if (path === '/schedule') return allow(method, 'GET'), upcomingRuns();
    // Sessions read the players dump here: this process holds Sleeper's copy for a day.
    if (path === '/players') return allow(method, 'GET'), [200, await config.client.players()];
    const run = /^\/jobs\/([^/]+)\/runs$/.exec(path);
    if (run) return allow(method, 'POST'), startOnDemand(safeDecode(run[1]!), url.searchParams);
    const m = /^\/leagues\/([^/]+)\/snapshots(?:\/([^/]+))?$/.exec(path);
    if (!m) throw new HttpError(404, 'not found');
    return snapshots(method, url.searchParams, safeDecode(m[1]!), m[2] === undefined ? undefined : safeDecode(m[2]));
  }

  // GET lists a league's stored dates, POST takes today's if it is missing, GET /<date> reads one.
  async function snapshots(method: string, query: URLSearchParams, league: string, date?: string): Promise<[number, unknown]> {
    if (date !== undefined) {
      allow(method, 'GET');
      const found = DATE.test(date) && store.snapshot(league, date);
      if (!found) throw new HttpError(404, 'no snapshot yet');
      return [200, found];
    }
    allow(method, 'GET', 'POST');
    if (method === 'POST') return [200, await takeSnapshot(config.client, store, league, now())];
    const limit = pageSizeOf(query.get('pageSize'));
    const rows = store.history(league, limit + 1, afterOf(query.get('pageToken')));
    const page = rows.slice(0, limit);
    return [200, { snapshots: page, ...(rows.length > limit && { nextPageToken: pageToken(page.at(-1)!.date) }) }];
  }

  // Sleeper's live week and the next run of each of this league's schedules, soonest first.
  async function upcomingRuns(): Promise<[number, unknown]> {
    if (!config.schedules || !config.league) throw new HttpError(501, 'schedules are not configured');
    const doFetch = config.schedules.fetch ?? fetch;
    const all: Array<{ id: string; nextRun: string | null }> = [];
    for (let token = ''; ;) {
      const res = await doFetch(`${config.schedules.url}?${new URLSearchParams({ pageSize: '1000', ...(token && { pageToken: token }) })}`);
      if (!res.ok) throw new HttpError(502, `schedules answered ${res.status}`);
      const page = (await res.json()) as { schedules: typeof all; nextPageToken?: string };
      all.push(...page.schedules);
      if (!page.nextPageToken) break;
      token = page.nextPageToken;
    }
    const league = config.league.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    const runs = all.flatMap((s) => {
      const m = new RegExp(`^sleeper/(lineup|waivers|trends|digest|retro|trade-finder)/${league}(?:/|$)`).exec(s.id);
      return m && s.nextRun ? [{ job: m[1]!, at: s.nextRun }] : [];
    });
    return [200, { week: (await config.client.state()).week, runs: runs.sort((a, b) => a.at.localeCompare(b.at)) }];
  }

  // POST /jobs/<job>/runs[?week=N][&requestId=R] starts a job on demand in the configured league; the week defaults to Sleeper's current one.
  async function startOnDemand(job: string, query: URLSearchParams): Promise<[number, unknown]> {
    if (!Object.hasOwn(PAGE_JOBS, job)) throw new HttpError(404, `no job named ${job}`);
    if (!config.league) throw new HttpError(501, 'no league configured');
    if (!config.distributorUrl || !config.owner) throw new HttpError(501, 'on-demand jobs are not configured');
    const week = query.get('week');
    if (week !== null && !(/^\d+$/.test(week) && Number(week) >= 1)) throw new HttpError(400, 'bad week');
    const requestId = query.get('requestId') ?? undefined;
    if (requestId === '') throw new HttpError(400, 'requestId must not be empty');
    const n = week === null ? (await config.client.state()).week : Number(week);
    const start = PAGE_JOBS[job as keyof typeof PAGE_JOBS] as (c: StartConfig, input: any) => ReturnType<typeof startLineup>;
    const r = await start({ distributorUrl: config.distributorUrl, fetch: config.fetch, now, requestId }, { league: config.league, week: n, owner: config.owner });
    if (!r.ok) throw new HttpError(502, `the distributor refused the job: HTTP ${r.status}`);
    return [202, { sessionKey: r.sessionKey }];
  }

  return {
    stop: () => store.close(),
    async handle(req, res) {
      try {
        const [status, body] = await route(req.method ?? 'GET', new URL(req.url ?? '', 'http://x'));
        send(res, status, body);
      } catch (err) {
        if (err instanceof HttpError && err.allow) res.setHeader('allow', err.allow);
        sendProblem(res, err instanceof HttpError ? err.status : 502, (err as Error).message);
      }
    },
  };
}
