import type { Json, SleeperClient } from './client.ts';
import type { Store } from './store.ts';

/** Calendar date in the league's clock (America/New_York), so one snapshot a day follows the owners' day, not UTC. */
export const leagueDate = (at: Date) => new Intl.DateTimeFormat('en-CA', { timeZone: 'America/New_York' }).format(at);

// The slice of a rostered player that day-to-day trends diff; the full dump is too large to keep daily.
const sliver = (p: Json) => ({ name: [p.first_name, p.last_name].filter(Boolean).join(' '), team: p.team ?? null, position: p.position ?? null, injury: p.injury_status ?? null, depth: p.depth_chart_order ?? null });

export async function buildSnapshot(client: SleeperClient, league: string) {
  const [state, info, users, rosters, dump] = await Promise.all([client.state(), client.league(league), client.users(league), client.rosters(league), client.players()]);
  const matchups = await client.matchups(league, state.week);
  const rostered = new Set<string>(rosters.flatMap((r: Json) => r.players ?? []));
  const players = Object.fromEntries([...rostered].filter((id) => dump[id]).map((id) => [id, sliver(dump[id])]));
  return { league: info, state, users, rosters, matchups, players };
}

/** Takes today's snapshot unless one is stored; returns the stored row either way. */
export async function takeSnapshot(client: SleeperClient, store: Store, league: string, at: Date) {
  const date = leagueDate(at);
  const have = store.snapshot(league, date);
  if (have) return have;
  store.saveSnapshot(league, date, at.toISOString(), await buildSnapshot(client, league));
  return store.snapshot(league, date)!;
}
