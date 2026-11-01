// Read-only Sleeper API client (SL-7): it only ever issues GET.
export type Json = any;

export class NotFound extends Error {}

export type ClientConfig = { baseUrl?: string; fetch?: typeof fetch; now?: () => number };

// State moves fastest; league data is held a little longer; the players dump is large and rate-limited, so a day.
const MINUTE = 60_000;
const TTL = { state: 5 * MINUTE, league: 10 * MINUTE, matchups: 2 * MINUTE, players: 24 * 60 * MINUTE, draft: 2 * MINUTE };

export function createSleeperClient(config: ClientConfig = {}) {
  const base = config.baseUrl ?? 'https://api.sleeper.app';
  const doFetch = config.fetch ?? fetch;
  const now = config.now ?? Date.now;
  const cache = new Map<string, { value: Json; expires: number }>();

  async function get(path: string, ttl: number): Promise<Json> {
    const hit = cache.get(path);
    if (hit && now() < hit.expires) return hit.value;
    const res = await doFetch(`${base}${path}`, { method: 'GET' });
    const text = await res.text();
    // Sleeper answers 200 with a literal null for unknown ids; never cache that.
    if (res.status === 404 || text.trim() === 'null') throw new NotFound(`sleeper: ${path} not found`);
    if (!res.ok) throw new Error(`sleeper: ${path} returned ${res.status}`);
    const value = JSON.parse(text);
    for (const [key, e] of cache) if (now() >= e.expires) cache.delete(key);
    cache.set(path, { value, expires: now() + ttl });
    return value;
  }

  return {
    state: () => get('/v1/state/nfl', TTL.state),
    league: (id: string) => get(`/v1/league/${id}`, TTL.league),
    users: (id: string) => get(`/v1/league/${id}/users`, TTL.league),
    rosters: (id: string) => get(`/v1/league/${id}/rosters`, TTL.league),
    matchups: (id: string, week: number) => get(`/v1/league/${id}/matchups/${week}`, TTL.matchups),
    transactions: (id: string, round: number) => get(`/v1/league/${id}/transactions/${round}`, TTL.league),
    drafts: (id: string) => get(`/v1/league/${id}/drafts`, TTL.league),
    draft: (id: string) => get(`/v1/draft/${id}`, TTL.draft),
    draftPicks: (id: string) => get(`/v1/draft/${id}/picks`, TTL.draft),
    players: () => get('/v1/players/nfl', TTL.players),
  };
}

export type SleeperClient = ReturnType<typeof createSleeperClient>;
