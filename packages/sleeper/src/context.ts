import { UNAVAILABLE, voidGame } from './games.ts';
import type { Json, SleeperClient } from './ports.ts';

/** Prior weeks behind a defense rank; recent form reads his team's last FORM_GAMES games of them. */
const DVP_WEEKS = 6;
const FORM_GAMES = 4;

const FORM_LEGEND = "After |: Ln = his points in his NFL team's last n games (up to 4), newest first (- = did not play); snap share; targets or touches a game";
export const CONTEXT_LEGEND = `${FORM_LEGEND}; vs OPP #n = OPP's PPR defense rank against his position over 6 weeks (1 allows fewest of 32); his NFL team's record, points a game and spread; who is out.`;
export const TRADE_LEGEND = `${FORM_LEGEND}; who is out.`;

/** A player's context as a row suffix; facts sealed before context existed carry none. */
export const contextTail = (context: Record<string, string> | undefined, id: string) => (context?.[id] ? ` | ${context[id]}` : '');

/** Prior weeks' stats (newest first) and this week's scores; a failed or missing week is dropped, never thrown. */
export type Recent = { weeks: Array<{ week: number; stats: Json }>; scores: Json[] };

export async function fetchRecent(client: SleeperClient, season: string, week: number): Promise<Recent> {
  const wanted = Array.from({ length: DVP_WEEKS }, (_, i) => week - 1 - i).filter((w) => w >= 1);
  const [scores, ...stats] = await Promise.all([client.scores(season, week).catch(() => []), ...wanted.map((w) => client.stats(season, w).catch(() => null))]);
  return { weeks: wanted.flatMap((w, i) => (stats[i] && typeof stats[i] === 'object' ? [{ week: w, stats: stats[i] }] : [])), scores: Array.isArray(scores) ? scores : [] };
}

const r1 = (n: number) => Math.round(n * 10) / 10;
const sum = (rows: Json[], f: (s: Json) => number) => rows.reduce((n, s) => n + (f(s) || 0), 0);

/** Rank 1-32 of each defense against `pos`, by Sleeper's per-game fantasy points allowed to it (PPR), fewest first. */
export function dvpRanks(weeks: Recent['weeks'], pos: string): Map<string, number> {
  const field = `fan_pts_allow_${pos.toLowerCase()}`;
  const by = new Map<string, number[]>();
  for (const { stats } of weeks) {
    for (const [team, s] of Object.entries<Json>(stats)) if (s?.gp > 0 && Number.isFinite(s[field])) by.set(team, [...(by.get(team) ?? []), s[field]]);
  }
  const avg = [...by].map(([team, v]) => [team, v.reduce((a, b) => a + b, 0) / v.length] as const).sort((a, b) => a[1] - b[1] || a[0].localeCompare(b[0]));
  return new Map(avg.map(([team], i) => [team, i + 1]));
}

type Ctx = { dump: Json; schedule: Json[]; key: string; week: number; recent: Recent; ranks: Map<string, Map<string, number>>; depth: Map<string, Json[]>; brief: boolean };
type TeamGame = { stats: Json; opp: string };

const opponent = (schedule: Json[], week: number, team: string): string | undefined => {
  const g = schedule.find((x) => x.week === week && (x.home === team || x.away === team) && !voidGame(x.status));
  return g && (g.home === team ? g.away : g.home);
};

/** His team's last FORM_GAMES games among the fetched weeks, newest first. */
const teamGames = (team: string, c: Ctx): TeamGame[] =>
  c.recent.weeks.flatMap(({ week, stats }) => { const opp = opponent(c.schedule, week, team); return opp ? [{ stats, opp }] : []; }).slice(0, FORM_GAMES);

function usage(pos: string, rows: Json[]): string[] {
  const out: string[] = [];
  const team = sum(rows, (s) => s.tm_off_snp);
  if (team > 0 && pos !== 'K') out.push(`snap ${Math.round((100 * sum(rows, (s) => s.off_snp)) / team)}%`);
  if (pos === 'RB') out.push(`${r1(sum(rows, (s) => (s.rush_att ?? 0) + (s.rec ?? 0)) / rows.length)} tch/g`);
  if (pos === 'WR' || pos === 'TE') out.push(`${r1(sum(rows, (s) => s.rec_tgt) / rows.length)} tgt/g`);
  return out;
}

function form(id: string, pos: string, games: TeamGame[], key: string): string[] {
  const mine = games.map((g) => (g.stats[id]?.gp > 0 ? g.stats[id] : null));
  const played = mine.filter((s): s is Json => s !== null);
  if (!played.length) return ['no recent data'];
  return [`L${games.length} ${mine.map((s) => (s ? Math.round(s[key] ?? 0) : '-')).join('/')}`, ...usage(pos, played)];
}

function matchup(pos: string, team: string, c: Ctx): string[] {
  const opp = opponent(c.schedule, c.week, team);
  if (!opp) return [];
  if (!c.ranks.has(pos)) c.ranks.set(pos, dvpRanks(c.recent.weeks, pos));
  const rank = c.ranks.get(pos)!.get(opp);
  return [`vs ${opp}${rank ? ` #${rank}` : ''}`];
}

/** Record and spread from this week's scores; team points per game from each opponent's points allowed. */
function teamLine(team: string, games: TeamGame[], c: Ctx): string {
  const g = c.recent.scores.find((s) => s.metadata?.home_team === team || s.metadata?.away_team === team)?.metadata;
  const record: string | undefined = (g?.home_team === team ? g?.home_record : g?.away_record)?.replace(/-0$/, '');
  const spread = g?.spread?.[team];
  const pts = games.map((x) => x.stats[x.opp]?.pts_allow).filter(Number.isFinite);
  return [record, pts.length ? `${Math.round(sum(pts, Number) / pts.length)}ppg` : undefined, Number.isFinite(spread) ? `${spread > 0 ? '+' : ''}${spread}` : undefined].filter(Boolean).join(' ');
}

const status = (p: Json) => `${p.last_name} ${p.injury_status}`;

/** The healthy QB at the top of the depth chart, naming any top-two QB who is out. */
function qbLine(team: string, c: Ctx): string[] {
  const qbs = (c.depth.get(team) ?? []).filter((p) => p.position === 'QB');
  const out = qbs.filter((p) => p.depth_chart_order <= 2 && UNAVAILABLE.has(p.injury_status));
  if (!out.length) return [];
  return [`QB ${qbs.find((p) => !UNAVAILABLE.has(p.injury_status))?.last_name ?? '?'} (${out.map(status).join(', ')})`];
}

const teammatesOut = (id: string, pos: string, team: string, c: Ctx): string[] =>
  (c.depth.get(team) ?? []).filter((p) => p.id !== id && p.position === pos && p.depth_chart_order <= 2 && UNAVAILABLE.has(p.injury_status)).map(status);

function lineOf(id: string, c: Ctx): string {
  const p = c.dump[id] ?? {};
  const pos: string = p.position ?? '';
  const team: string | undefined = p.team ?? (pos === 'DEF' ? id : undefined);
  if (!team) return 'no recent data';
  const games = teamGames(team, c);
  const qb = pos === 'QB' || pos === 'DEF' ? [] : qbLine(team, c);
  const week = c.brief ? [] : [...matchup(pos, team, c), teamLine(team, games, c)];
  return [...form(id, pos, games, c.key), ...week, ...qb, ...teammatesOut(id, pos, team, c)].filter(Boolean).join(', ');
}

/** Players with a depth-chart spot, by NFL team, depth order first. */
function depthByTeam(dump: Json): Map<string, Json[]> {
  const by = new Map<string, Json[]>();
  for (const [id, p] of Object.entries<Json>(dump)) if (p?.team && Number.isFinite(p.depth_chart_order)) by.set(p.team, [...(by.get(p.team) ?? []), { ...p, id }]);
  for (const list of by.values()) list.sort((a, b) => a.depth_chart_order - b.depth_chart_order);
  return by;
}

/** One compact line per player: recent form, usage, opponent defense rank, team quality, spread and who around him is out. `brief` keeps only form, usage and who is out. */
export function contextLines(ids: string[], d: { dump: Json; schedule: Json[]; key: string; week: number; recent: Recent; brief?: boolean }): Record<string, string> {
  const c: Ctx = { ...d, ranks: new Map(), depth: depthByTeam(d.dump), brief: d.brief ?? false };
  return Object.fromEntries([...new Set(ids)].map((id) => [id, lineOf(id, c)]));
}

/** Per position, how many free agents reach `level` there, e.g. "K: 19 FA ≥ 7". */
export function faDepth<P extends { pos: string }>(free: P[], level: Record<string, number>, value: (p: P) => number | null): string[] {
  return Object.entries(level).map(([pos, bar]) => `${pos}: ${free.filter((p) => p.pos === pos && (value(p) ?? 0) >= bar).length} FA ≥ ${r1(bar)}`);
}
