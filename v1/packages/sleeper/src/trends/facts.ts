import type { Json, SleeperClient } from '../client.ts';
import type { LineupInput, NewsItem } from '../lineup/facts.ts';
import type { Searchable } from '../lineup/news.ts';

/** A player as the trends job names him; `depth` is Sleeper's depth_chart_order. */
export type TrendPlayer = { id: string; name: string; pos: string | null; team: string | null; inj: string | null; depth: number | null };
/** One Sleeper field that moved between two snapshots. */
export type Change = { playerId: string; field: 'injury' | 'depth'; from: string | number | null; to: string | number | null };
export type Trending = { playerId: string; adds: number };

/** What the trends job computed in code and the model writes from; the rubric's gates check the card against it. */
export type TrendsFacts = {
  league: string;
  week: number;
  /** The snapshot day this run reads, and the earlier one it is diffed against (null on the first day). */
  date: string;
  since: string | null;
  changes: Change[];
  /** Top 24-hour adds of players no team in the league holds. */
  trending: Trending[];
  /** Every player the changes and trending list name, by id. */
  players: Record<string, TrendPlayer>;
  news: Record<string, NewsItem[]>;
};

export type SnapshotRef = { date: string; takenAt: string };
export type TrendsFactsDeps = {
  client: SleeperClient;
  history: (league: string) => Promise<SnapshotRef[]>;
  snapshotOn: (league: string, date: string) => Promise<Json>;
  news?: (p: Searchable, at: number) => Promise<NewsItem[]>;
};

const TRENDING_LIMIT = 25;
export const TOP_TRENDING = 5;
/** Searches per run: each costs an Exa call, and a day rarely moves more injuries than this. */
export const MAX_SEARCHED = 10;
const POSITIONS = new Set(['QB', 'RB', 'WR', 'TE', 'K', 'DEF']);

const named = (p: Json) => [p.first_name, p.last_name].filter(Boolean).join(' ');

/** Injury and depth moves of players rostered on both days; a newly rostered player has no yesterday to move from. */
export function diffSnapshots(prev: Json, cur: Json): Change[] {
  const out: Change[] = [];
  for (const [id, now] of Object.entries<Json>(cur.players)) {
    const was = prev.players[id];
    if (!was) continue;
    if ((was.injury ?? null) !== (now.injury ?? null)) out.push({ playerId: id, field: 'injury', from: was.injury ?? null, to: now.injury ?? null });
    if ((was.depth ?? null) !== (now.depth ?? null)) out.push({ playerId: id, field: 'depth', from: was.depth ?? null, to: now.depth ?? null });
  }
  return out.sort((a, b) => Number(b.field === 'injury') - Number(a.field === 'injury') || a.playerId.localeCompare(b.playerId));
}

function trendingFree(trending: Json[], dump: Json, rostered: Set<string>): Trending[] {
  return trending
    .filter((t) => !rostered.has(t.player_id) && dump[t.player_id]?.team && POSITIONS.has(dump[t.player_id].position))
    .slice(0, TOP_TRENDING)
    .map((t) => ({ playerId: t.player_id, adds: t.count }));
}

/** Today's snapshot against the last earlier one, the free agents rising on Sleeper, and dated news for the players that moved. */
export async function buildTrendsFacts(deps: TrendsFactsDeps, snap: Json, input: LineupInput, at: number): Promise<TrendsFacts> {
  const earlier = (await deps.history(input.league)).find((h) => h.date < snap.date);
  const prev = earlier && (await deps.snapshotOn(input.league, earlier.date));
  const [dump, trending] = await Promise.all([deps.client.players(), deps.client.trending('add', 24, TRENDING_LIMIT)]);
  const rostered = new Set<string>(snap.data.rosters.flatMap((r: Json) => r.players ?? []));
  const changes = prev ? diffSnapshots(prev.data, snap.data) : [];
  const rising = trendingFree(trending, dump, rostered);
  const players: Record<string, TrendPlayer> = {};
  const add = (id: string, p: { name: string; position?: string; team?: string; injury?: string; depth?: number }) =>
    (players[id] = { id, name: p.name, pos: p.position ?? null, team: p.team ?? null, inj: p.injury ?? null, depth: p.depth ?? null });
  for (const c of changes) add(c.playerId, snap.data.players[c.playerId]);
  for (const t of rising) add(t.playerId, { ...dump[t.playerId], name: named(dump[t.playerId]), injury: dump[t.playerId].injury_status, depth: dump[t.playerId].depth_chart_order });
  const facts: TrendsFacts = { league: input.league, week: input.week, date: snap.date, since: earlier?.date ?? null, changes, trending: rising, players, news: {} };
  if (deps.news) for (const id of [...new Set(changes.map((c) => c.playerId))].slice(0, MAX_SEARCHED)) facts.news[id] = await deps.news({ name: players[id].name, team: players[id].team, pos: players[id].pos ?? '' }, at);
  return facts;
}
