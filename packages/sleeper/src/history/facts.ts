import { NotFound, type Json, type SleeperClient } from '../client.ts';
import { ownerTeam, ptsKey, round2, type LineupInput } from '../lineup/facts.ts';
import { bestLineup, lineupSlots } from '../lineup/slots.ts';

/** Position-rank gap at which a pick counts as a reach or a steal: sleeper_history's rule in v0, the one definition (audit contradiction 2). */
export const REACH_STEAL_GAP = 5;
/** A loss by fewer than this many points is a close loss. */
export const CLOSE_LOSS_MARGIN = 10;
const MAX_SEASONS = 10;
const LAST_WEEK = 18;

/** Declared on every card, so the reader and the judge see the one definition behind each figure. */
export const DEFINITIONS = {
  reach_steal: `Within a position, a drafted player's draft rank (by pick number) is compared with his finish rank (by full-season points in this league's scoring, among the players drafted at that position). Finishing ${REACH_STEAL_GAP} or more ranks below the draft rank is a reach, ${REACH_STEAL_GAP} or more above is a steal, anything between is as_expected.`,
  efficiency: 'Season points scored divided by the best possible points Sleeper reports for the season, never an average of weekly percentages.',
  close_loss: `A loss by fewer than ${CLOSE_LOSS_MARGIN} points.`,
  left: 'Each week, the best legal lineup from the whole roster minus the lineup started.',
} as const;

export type Verdict = 'reach' | 'steal' | 'as_expected' | 'unknown';
export type DraftFigure = { round: number; pick: number; player: { id: string; name: string; pos: string }; drafted_as: number; finished: number; pts: number; verdict: Verdict };
export type WeekFigure = { week: number; result: 'W' | 'L' | 'T' | null; started: number; best: number; left: number; opp: number | null; close_loss: boolean };
export type SeasonFigures = {
  season: string;
  league_id: string;
  in_progress: boolean;
  wins: number;
  losses: number;
  ties: number;
  pf: number;
  pa: number;
  /** 1 is best: the most points for, the fewest against. */
  pf_rank: number;
  pa_rank: number;
  eff: number | null;
  left_total: number;
  close_losses: number;
  champion: string | null;
  weeks: WeekFigure[];
  /** Null while the season is in progress: there is no finish to grade against. */
  draft: DraftFigure[] | null;
};
export type HistoryFacts = { league: string; week: number; team: string; definitions: typeof DEFINITIONS; seasons: SeasonFigures[] };
export type HistoryInput = LineupInput & { seasons?: number };

export function verdictOf(draftedAs: number, finished: number): Verdict {
  if (!draftedAs || !finished) return 'unknown';
  const gap = finished - draftedAs;
  return gap >= REACH_STEAL_GAP ? 'reach' : gap <= -REACH_STEAL_GAP ? 'steal' : 'as_expected';
}

const pts = (settings: Json, field: string) => (settings?.[field] ?? 0) + (settings?.[`${field}_decimal`] ?? 0) / 100;
const settled = <T>(p: Promise<T[]>): Promise<T[]> => p.catch((e) => { if (e instanceof NotFound) return []; throw e; });

/** The league and the seasons before it, newest first; a missing older season ends the chain. */
async function chain(client: SleeperClient, id: string, back: number): Promise<Json[]> {
  const out: Json[] = [];
  for (let next: string | null = id; next && out.length < back; ) {
    const league: Json | undefined = await client.league(next).catch((e) => { if (e instanceof NotFound && out.length) return undefined; throw e; });
    if (!league) break;
    out.push(league);
    next = league.previous_league_id && league.previous_league_id !== '0' ? league.previous_league_id : null;
  }
  return out;
}

/** 1-based rank of a roster by a number: first place has the most when `high`, the least otherwise. */
function rankOf(rosters: Json[], id: number, value: (r: Json) => number, high: boolean): number {
  const mine = value(rosters.find((r) => r.roster_id === id)!);
  return 1 + rosters.filter((r) => (high ? value(r) > mine : value(r) < mine)).length;
}

const resultOf = (mine: number, opp: number): 'W' | 'L' | 'T' => (mine > opp ? 'W' : mine < opp ? 'L' : 'T');

function weekFigure(week: number, league: Json, mine: Json, opp: Json | undefined, dump: Json): WeekFigure {
  const points: Record<string, number> = mine.players_points ?? {};
  const pool = (mine.players ?? []).map((id: string) => ({ id, pos: dump[id]?.position ?? '', pts: points[id] ?? 0 }));
  const best = round2(bestLineup(lineupSlots(league.roster_positions), pool).reduce((n, id) => n + (id ? points[id] ?? 0 : 0), 0));
  const result = opp ? resultOf(mine.points, opp.points) : null;
  return { week, result, started: round2(mine.points), best, left: round2(best - mine.points), opp: opp ? round2(opp.points) : null, close_loss: result === 'L' && opp!.points - mine.points < CLOSE_LOSS_MARGIN };
}

/** Regular-season weeks only: wins and losses in Sleeper's roster settings stop where the playoffs start. */
function lastWeek(league: Json, currentWeek: number): number {
  const playoffs = league.settings?.playoff_week_start;
  const played = league.status === 'complete' ? LAST_WEEK : currentWeek - 1;
  return Math.min(played, playoffs ? playoffs - 1 : LAST_WEEK);
}

async function weekFigures(client: SleeperClient, league: Json, rosterId: number, dump: Json, currentWeek: number): Promise<WeekFigure[]> {
  const last = lastWeek(league, currentWeek);
  const all = await Promise.all(Array.from({ length: Math.max(last, 0) }, (_, i) => settled(client.matchups(league.league_id, i + 1))));
  return all.flatMap((matchups: Json[], i) => {
    const mine = matchups.find((m) => m.roster_id === rosterId);
    const opp = mine && matchups.find((m) => m.matchup_id === mine.matchup_id && m.roster_id !== rosterId);
    return mine ? [weekFigure(i + 1, league, mine, opp, dump)] : [];
  });
}

const position = (pick: Json): string => pick.metadata?.position ?? '';
const nameOf = (pick: Json) => [pick.metadata?.first_name, pick.metadata?.last_name].filter(Boolean).join(' ') || pick.player_id;

/** Rank of every pick within its position under `order`. */
function positionalRanks(picks: Json[], order: (a: Json, b: Json) => number): Map<string, number> {
  const byPos = Map.groupBy(picks, position);
  return new Map([...byPos].flatMap(([, group]) => [...group].sort(order).map((p, i): [string, number] => [p.player_id, i + 1])));
}

async function draftFigures(client: SleeperClient, league: Json, rosterId: number): Promise<DraftFigure[] | null> {
  if (league.status !== 'complete' || !league.draft_id) return null;
  const [picks, stats] = await Promise.all([client.draftPicks(league.draft_id) as Promise<Json[]>, client.seasonStats(league.season)]);
  const key = ptsKey(league.scoring_settings?.rec);
  const total = (p: Json): number => stats[p.player_id]?.[key] ?? 0;
  const drafted = positionalRanks(picks, (a, b) => a.pick_no - b.pick_no);
  const finished = positionalRanks([...picks].sort((a, b) => a.pick_no - b.pick_no), (a, b) => total(b) - total(a));
  return picks.filter((p) => p.roster_id === rosterId).sort((a, b) => a.pick_no - b.pick_no).map((p) => {
    const [d, f] = [drafted.get(p.player_id) ?? 0, finished.get(p.player_id) ?? 0];
    return { round: p.round, pick: p.pick_no, player: { id: p.player_id, name: nameOf(p), pos: position(p) }, drafted_as: d, finished: f, pts: round2(total(p)), verdict: position(p) ? verdictOf(d, f) : 'unknown' };
  });
}

async function champion(client: SleeperClient, league: Json, names: Map<number, string>): Promise<string | null> {
  const bracket: Json[] = await settled(client.winnersBracket(league.league_id));
  const final = bracket.find((m) => m.p === 1 && m.w != null);
  return final ? names.get(final.w) ?? null : null;
}

async function seasonOf(client: SleeperClient, league: Json, owner: string, dump: Json, currentWeek: number): Promise<SeasonFigures | null> {
  const [users, rosters] = await Promise.all([client.users(league.league_id), client.rosters(league.league_id)]);
  const found = (() => { try { return ownerTeam({ data: { users, rosters } }, owner); } catch { return null; } })();
  if (!found) return null;
  const { roster } = found;
  const s = roster.settings ?? {};
  const names = new Map<number, string>(rosters.map((r: Json) => { const u = users.find((x: Json) => x.user_id === r.owner_id); return [r.roster_id, u?.metadata?.team_name ?? u?.display_name ?? `roster ${r.roster_id}`]; }));
  const weeks = await weekFigures(client, league, roster.roster_id, dump, currentWeek);
  const ppts = pts(s, 'ppts');
  return {
    season: league.season,
    league_id: league.league_id,
    in_progress: league.status !== 'complete',
    wins: s.wins ?? 0,
    losses: s.losses ?? 0,
    ties: s.ties ?? 0,
    pf: round2(pts(s, 'fpts')),
    pa: round2(pts(s, 'fpts_against')),
    pf_rank: rankOf(rosters, roster.roster_id, (r) => pts(r.settings, 'fpts'), true),
    pa_rank: rankOf(rosters, roster.roster_id, (r) => pts(r.settings, 'fpts_against'), false),
    eff: ppts ? Math.round((pts(s, 'fpts') / ppts) * 1000) / 10 : null,
    left_total: round2(weeks.reduce((n, w) => n + w.left, 0)),
    close_losses: weeks.filter((w) => w.close_loss).length,
    champion: await champion(client, league, names),
    weeks,
    draft: await draftFigures(client, league, roster.roster_id),
  };
}

/** Walks the league's season chain, newest first, and builds every figure the card reports. */
export async function buildHistoryFacts(client: SleeperClient, input: HistoryInput): Promise<HistoryFacts> {
  const [state, leagues, dump] = await Promise.all([client.state(), chain(client, input.league, Math.min(input.seasons ?? MAX_SEASONS, MAX_SEASONS)), client.players()]);
  const seasons = (await Promise.all(leagues.map((l) => seasonOf(client, l, input.owner, dump, state.week)))).filter((s): s is SeasonFigures => s !== null);
  if (!seasons.length) throw new Error(`sleeper: no team for ${input.owner} in any season of league ${input.league}`);
  const users = await client.users(input.league);
  const mine = users.find((u: Json) => [u.user_id, u.display_name, u.username].includes(input.owner));
  return { league: input.league, week: input.week, team: mine?.metadata?.team_name ?? mine?.display_name ?? input.owner, definitions: DEFINITIONS, seasons };
}
