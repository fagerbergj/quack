import type { Json } from '../client.ts';
import { round2 } from '../lineup/facts.ts';

export type Phase = 'preview' | 'recap' | 'live';
export type Standing = { rank: number; rosterId: number; team: string; wins: number; losses: number; ties: number; fpts: number; fpts_against: number; in_playoff_line: boolean };

const SKIPPED = ['canceled', 'postponed'];

/** Final once every game is complete or Sleeper has moved on; a preview only while no game has started. */
export function weekPhase(schedule: Json[], week: number, stateWeek: number): Phase {
  const games = schedule.filter((g) => g.week === week && !SKIPPED.includes(g.status));
  if (week < stateWeek || (games.length > 0 && games.every((g) => g.status === 'complete'))) return 'recap';
  return games.some((g) => g.status === 'in_game' || g.status === 'complete') ? 'live' : 'preview';
}

export const playerName = (dump: Json, id: string): string => [dump[id]?.first_name, dump[id]?.last_name].filter(Boolean).join(' ') || id;
/** A defense is its own team code in Sleeper's dump. */
export const teamOf = (dump: Json, id: string): string | null => dump[id]?.team ?? (dump[id]?.position === 'DEF' ? id : null);

export function teamNames(users: Json[], rosters: Json[]): Map<number, string> {
  const byUser = new Map<string, string>(users.map((u) => [u.user_id, u.metadata?.team_name ?? u.display_name]));
  return new Map(rosters.map((r) => [r.roster_id, byUser.get(r.owner_id) ?? `Team ${r.roster_id}`]));
}

const points = (s: Json, key: string) => (s?.[key] ?? 0) + (s?.[`${key}_decimal`] ?? 0) / 100;
export const record = (r: Json) => `${r.settings?.wins ?? 0}-${r.settings?.losses ?? 0}${r.settings?.ties ? `-${r.settings.ties}` : ''}`;

/** What v0's `sleeper_standings` returned: wins then points for, the playoff line at `playoff_teams`. */
export function standings(league: Json, rosters: Json[], names: Map<number, string>): { rows: Standing[]; playoffTeams: number } {
  const playoffTeams = league.settings?.playoff_teams ?? 0;
  const rows = rosters.map((r) => ({ rosterId: r.roster_id as number, team: names.get(r.roster_id)!, wins: r.settings?.wins ?? 0, losses: r.settings?.losses ?? 0, ties: r.settings?.ties ?? 0, fpts: round2(points(r.settings, 'fpts')), fpts_against: round2(points(r.settings, 'fpts_against')) }));
  rows.sort((a, b) => b.wins - a.wins || b.fpts - a.fpts);
  return { rows: rows.map((r, i) => ({ ...r, rank: i + 1, in_playoff_line: i + 1 <= playoffTeams })), playoffTeams };
}

/** Weeks left before the playoffs start, never negative. */
export const remainingWeeks = (league: Json, stateWeek: number) => Math.max(0, (league.settings?.playoff_week_start ?? 0) - stateWeek);

/** Kickoff instant (ms) by NFL team for one week, joined on game_id. */
export function kickoffs(schedule: Json[], scores: Json[], week: number): Map<string, number> {
  const starts = new Map<string, number>(scores.filter((s) => s.start_time > 0).map((s) => [s.game_id, s.start_time]));
  const out = new Map<string, number>();
  for (const g of schedule.filter((x) => x.week === week && !SKIPPED.includes(x.status) && starts.has(x.game_id))) for (const t of [g.home, g.away]) out.set(t, starts.get(g.game_id)!);
  return out;
}
