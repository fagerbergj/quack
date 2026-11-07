// Read-only Sleeper API client: it only ever issues GET.
import { limiter } from '@quack/judge';
import { sleepFor } from './limit.ts';
import type { Json, SleeperClient } from './ports.ts';

export type { Json, SleeperClient } from './ports.ts';

export class NotFound extends Error {}

export type ClientConfig = {
  baseUrl?: string;
  fetch?: typeof fetch;
  now?: () => number;
  /** Where the players dump comes from instead of Sleeper: a session reads the app's day-long copy. */
  players?: () => Promise<Json>;
  sleep?: (ms: number) => Promise<unknown>;
};

// State moves fastest; league data is held a little longer; the players dump is large and rate-limited, so a day.
const MINUTE = 60_000;
const TTL = { state: 5 * MINUTE, league: 10 * MINUTE, matchups: 2 * MINUTE, players: 24 * 60 * MINUTE, draft: 2 * MINUTE, projections: 60 * MINUTE, schedule: 2 * MINUTE, scores: 2 * MINUTE, trending: 5 * MINUTE, research: 60 * MINUTE, stats: 60 * MINUTE };

/** Sleeper allows about 1000 calls a minute per IP; a history walk issues hundreds, so they queue here. */
export const CONCURRENCY = 8;
export const ATTEMPTS = 4;
export const TIMEOUT_MS = 15_000;
/** The multi-megabyte players dump gets longer. */
const PLAYERS_TIMEOUT_MS = 120_000;
const MAX_WAIT_MS = 60_000;
const RETRIED = new Set([429, 500, 502, 503, 504]);

type Reply = { status: number; text: string; retryAfter: string | null };

/** Retry-After in seconds or as an HTTP date, else doubling from 1 s; never past MAX_WAIT_MS. */
function backoff(reply: Reply | undefined, attempt: number, now: number): number {
  const header = reply?.retryAfter;
  const asked = header === null || header === undefined ? NaN : /^\d+$/.test(header.trim()) ? Number(header) * 1000 : Date.parse(header) - now;
  return Math.min(MAX_WAIT_MS, Math.max(0, Number.isFinite(asked) ? asked : 1000 * 2 ** (attempt - 1)));
}

export function createSleeperClient(config: ClientConfig = {}): SleeperClient {
  const base = config.baseUrl ?? 'https://api.sleeper.app';
  const doFetch = config.fetch ?? fetch;
  const now = config.now ?? Date.now;
  const sleep = config.sleep ?? sleepFor;
  const slot = limiter(CONCURRENCY);
  const cache = new Map<string, { value: Json; expires: number }>();
  const pending = new Map<string, Promise<Json>>();

  // The timer is a plain setTimeout, so a test's mocked clock drives it.
  async function once(path: string): Promise<Reply> {
    const abort = new AbortController();
    const timer = setTimeout(() => abort.abort(new Error(`sleeper: ${path} timed out`)), path === '/v1/players/nfl' ? PLAYERS_TIMEOUT_MS : TIMEOUT_MS);
    try {
      const res = await doFetch(`${base}${path}`, { method: 'GET', signal: abort.signal });
      return { status: res.status, text: await res.text(), retryAfter: res.headers.get('retry-after') };
    } finally {
      clearTimeout(timer);
    }
  }

  async function fetchJson(path: string): Promise<Json> {
    for (let attempt = 1; ; attempt++) {
      let reply: Reply | undefined;
      let failure: unknown;
      try {
        reply = await slot(() => once(path));
      } catch (err) {
        failure = err;
      }
      if (reply && !RETRIED.has(reply.status)) {
        // Sleeper answers 200 with a literal null for unknown ids; never cache that.
        if (reply.status === 404 || reply.text.trim() === 'null') throw new NotFound(`sleeper: ${path} not found`);
        if (reply.status >= 400) throw new Error(`sleeper: ${path} returned ${reply.status}`);
        return JSON.parse(reply.text);
      }
      if (attempt >= ATTEMPTS) throw new Error(`sleeper: ${path} ${reply ? `returned ${reply.status}` : `failed: ${(failure as Error)?.message}`} after ${attempt} attempts`);
      await sleep(backoff(reply, attempt, now()));
    }
  }

  // Concurrent callers of one path share its request; a failure is not cached.
  function get(path: string, ttl: number, load: () => Promise<Json> = () => fetchJson(path)): Promise<Json> {
    const hit = cache.get(path);
    if (hit && now() < hit.expires) return Promise.resolve(hit.value);
    const inflight = pending.get(path);
    if (inflight) return inflight;
    const p = load().then((value) => {
      for (const [key, e] of cache) if (now() >= e.expires) cache.delete(key);
      cache.set(path, { value, expires: now() + ttl });
      return value;
    }).finally(() => pending.delete(path));
    pending.set(path, p);
    return p;
  }

  return {
    state: () => get('/v1/state/nfl', TTL.state),
    league: (id) => get(`/v1/league/${id}`, TTL.league),
    users: (id) => get(`/v1/league/${id}/users`, TTL.league),
    rosters: (id) => get(`/v1/league/${id}/rosters`, TTL.league),
    matchups: (id, week) => get(`/v1/league/${id}/matchups/${week}`, TTL.matchups),
    transactions: (id, round) => get(`/v1/league/${id}/transactions/${round}`, TTL.league),
    drafts: (id) => get(`/v1/league/${id}/drafts`, TTL.league),
    draft: (id) => get(`/v1/draft/${id}`, TTL.draft),
    draftPicks: (id) => get(`/v1/draft/${id}/picks`, TTL.draft),
    winnersBracket: (id) => get(`/v1/league/${id}/winners_bracket`, TTL.league),
    seasonStats: (season) => get(`/v1/stats/nfl/regular/${season}`, TTL.players),
    projections: (season, week) => get(`/v1/projections/nfl/regular/${season}/${week}`, TTL.projections),
    schedule: (season) => get(`/schedule/nfl/regular/${season}`, TTL.schedule),
    scores: (season, week) => get(`/scores/nfl/regular/${season}/${week}`, TTL.scores),
    players: () => get('/v1/players/nfl', TTL.players, config.players),
    trending: (type, hours, limit) => get(`/v1/players/nfl/trending/${type}?lookback_hours=${hours}&limit=${limit}`, TTL.trending),
    stats: (season, week) => get(`/v1/stats/nfl/regular/${season}/${week}`, TTL.stats),
    research: (season, week) => get(`/players/nfl/research/regular/${season}/${week}`, TTL.research),
  };
}
