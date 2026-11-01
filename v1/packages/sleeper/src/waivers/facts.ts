import type { Json, SleeperClient } from '../client.ts';
import { ownerTeam, ptsKey, round2, type LineupInput, type NewsItem } from '../lineup/facts.ts';
import type { Searchable } from '../lineup/news.ts';
import { fits, lineupSlots } from '../lineup/slots.ts';

export type WaiverType = 'rolling' | 'reverse_standings' | 'faab';
export type WPlayer = { id: string; name: string; pos: string; team: string | null; inj: string | null; proj: number; bye: number | null };
/** `owned` is Sleeper's platform-wide ownership percent and `adds` its 24-hour add count, null when the player is not on the trending list: scarcity signals, not value. */
export type FreeAgent = WPlayer & { owned: number | null; adds: number | null };
export type Mine = WPlayer & { role: 'starter' | 'bench' | 'ir' };
export type Bye = { week: number; pos: string; out: number };
/** One league-competition row: what the team lacks, what it can spend and where a bye leaves it short. */
export type TeamRow = { rosterId: number; team: string; mine: boolean; priority: number | null; faabLeft: number | null; needs: Record<string, number>; byes: Bye[] };

/** What the waivers job computed in code and the model reasons over; the rubric's gates check the card against it. */
export type WaiverFacts = {
  league: string;
  week: number;
  team: string;
  waiverType: WaiverType;
  myPriority: number | null;
  /** Null unless the league runs FAAB: Sleeper reports a budget in every league. */
  faabLeft: number | null;
  teams: number;
  table: TeamRow[];
  free: Record<string, FreeAgent>;
  /** Free-agent ids by position, best projection first. */
  buckets: Record<string, string[]>;
  /** Free agents above the ownership or trending cut: each must be a claim or passed over with a reason. */
  widely: string[];
  roster: Record<string, Mine>;
  news: Record<string, NewsItem[]>;
};

export type WaiverDeps = { client: SleeperClient; news?: (p: Searchable, at: number) => Promise<NewsItem[]> };

const WAIVER_TYPES: WaiverType[] = ['rolling', 'reverse_standings', 'faab'];
export const POSITIONS = ['QB', 'RB', 'WR', 'TE', 'K', 'DEF'];
export const UNAVAILABLE = new Set(['Out', 'IR', 'Doubtful', 'Sus']);
/** Skill positions want one bench cover beyond their starting slots. */
export const COVER: Record<string, number> = { RB: 1, WR: 1, TE: 1 };
export const TOP_PER_POS = 5;
export const WIDE_OWNED = 40;
const TRENDING_LIMIT = 25;
/** Weeks ahead a bye still counts as inside the lead time to cover it. */
export const BYE_LEAD = 2;
const CLOSE_GAP = 3;

/** Each NFL team's bye: the schedule week it has no game. */
export function byeWeeks(schedule: Json[]): Map<string, number> {
  const weeks = [...new Set<number>(schedule.map((g) => g.week))];
  const playing = new Map<string, Set<number>>();
  for (const g of schedule) for (const t of [g.home, g.away]) playing.set(t, (playing.get(t) ?? new Set()).add(g.week));
  return new Map([...playing].flatMap(([t, w]): Array<[string, number]> => { const bye = weeks.find((x) => !w.has(x)); return bye === undefined ? [] : [[t, bye]]; }));
}

export function wplayer(id: string, dump: Json, proj: Json, key: string, byes: Map<string, number>): WPlayer {
  const p = dump[id] ?? {};
  const pos = p.position ?? '';
  const team: string | null = p.team ?? (pos === 'DEF' ? id : null);
  return { id, name: [p.first_name, p.last_name].filter(Boolean).join(' ') || id, pos, team, inj: p.injury_status ?? null, proj: round2(proj[id]?.[key] ?? 0), bye: (team && byes.get(team)) || null };
}

const settingsOf = (r: Json) => r.settings ?? {};

/** Positions short of the starting slots (plus a skill-position backup), and the weeks a bye leaves the team short. */
export function teamNeeds(players: WPlayer[], slots: string[], positions: string[], week: number): Pick<TeamRow, 'needs' | 'byes'> {
  const healthy = players.filter((p) => !UNAVAILABLE.has(p.inj ?? ''));
  const required = (pos: string) => slots.filter((s) => s === pos).length;
  const needs = Object.fromEntries(positions.flatMap((pos) => {
    const short = required(pos) + (COVER[pos] ?? 0) - healthy.filter((p) => p.pos === pos).length;
    return short > 0 ? [[pos, short]] : [];
  }));
  const byes = Array.from({ length: BYE_LEAD + 1 }, (_, i) => week + i).flatMap((w) => positions.flatMap((pos) => {
    const here = healthy.filter((p) => p.pos === pos);
    const out = here.filter((p) => p.bye === w).length;
    return out > 0 && here.length - out < required(pos) ? [{ week: w, pos, out }] : [];
  }));
  return { needs, byes };
}

function teamRows(rosters: Json[], users: Json[], deps: { type: WaiverType; budget: number; slots: string[]; positions: string[]; week: number; mineId: number; wp: (id: string) => WPlayer }): TeamRow[] {
  return rosters.map((r) => {
    const benched = new Set<string>([...(r.reserve ?? []), ...(r.taxi ?? [])]);
    const players = (r.players ?? []).filter((id: string) => !benched.has(id)).map(deps.wp);
    const user = users.find((u) => u.user_id === r.owner_id);
    return {
      rosterId: r.roster_id,
      team: user?.metadata?.team_name ?? user?.display_name ?? `roster ${r.roster_id}`,
      mine: r.roster_id === deps.mineId,
      priority: settingsOf(r).waiver_position ?? null,
      faabLeft: deps.type === 'faab' ? deps.budget - (settingsOf(r).waiver_budget_used ?? 0) : null,
      ...teamNeeds(players, deps.slots, deps.positions, deps.week),
    };
  });
}

function freeAgents(dump: Json, taken: Set<string>, positions: string[], wp: (id: string) => WPlayer, trending: Map<string, number>, research: Json) {
  const free: Record<string, FreeAgent> = {};
  for (const [id, p] of Object.entries<Json>(dump)) {
    if (taken.has(id) || p.active === false || !positions.includes(p.position)) continue;
    const w = wp(id);
    if (w.team) free[id] = { ...w, owned: research[id]?.owned ?? null, adds: trending.get(id) ?? null };
  }
  const all = Object.values(free).sort((a, b) => b.proj - a.proj || a.id.localeCompare(b.id));
  const widely = all.filter((p) => (p.owned ?? 0) >= WIDE_OWNED || (p.adds ?? 0) > 0).map((p) => p.id);
  const buckets = Object.fromEntries(positions.map((pos) => {
    const top = all.filter((p) => p.pos === pos).slice(0, TOP_PER_POS).map((p) => p.id);
    return [pos, [...new Set([...top, ...widely.filter((id) => free[id].pos === pos)])].sort((a, b) => free[b].proj - free[a].proj || a.localeCompare(b))];
  }));
  return { free, buckets, widely };
}

export async function buildWaiverFacts(deps: WaiverDeps, snap: Json, input: LineupInput, at: number): Promise<WaiverFacts> {
  const { league, users, rosters } = snap.data;
  const { user, roster } = ownerTeam(snap, input.owner);
  const { client } = deps;
  const [dump, proj, schedule, adds, research] = await Promise.all([
    client.players(), client.projections(league.season, input.week), client.schedule(league.season), client.trending('add', 24, TRENDING_LIMIT), client.research(league.season, input.week),
  ]);
  const type = WAIVER_TYPES[league.settings?.waiver_type ?? 0];
  if (!type) throw new Error(`sleeper: unknown waiver_type ${league.settings?.waiver_type}`);
  const key = ptsKey(league.scoring_settings?.rec);
  const byes = byeWeeks(schedule);
  const wp = (id: string) => wplayer(id, dump, proj, key, byes);
  const slots = lineupSlots(league.roster_positions);
  const positions = POSITIONS.filter((pos) => slots.some((s) => fits(s, pos)));
  const budget = league.settings?.waiver_budget ?? 0;
  const table = teamRows(rosters, users, { type, budget, slots, positions, week: input.week, mineId: roster.roster_id, wp });
  const taken = new Set<string>(rosters.flatMap((r: Json) => r.players ?? []));
  const starters = new Set<string>(roster.starters ?? []);
  const ir = new Set<string>(roster.reserve ?? []);
  const mine = Object.fromEntries((roster.players as string[]).filter((id) => dump[id]).map((id) => [id, { ...wp(id), role: ir.has(id) ? 'ir' : starters.has(id) ? 'starter' : 'bench' } as Mine]));
  const facts: WaiverFacts = {
    league: input.league,
    week: input.week,
    team: user.metadata?.team_name ?? user.display_name,
    waiverType: type,
    myPriority: settingsOf(roster).waiver_position ?? null,
    faabLeft: table.find((t) => t.mine)!.faabLeft,
    teams: rosters.length,
    table,
    ...freeAgents(dump, taken, positions, wp, new Map(adds.map((t: Json) => [t.player_id, t.count])), research),
    roster: mine,
    news: {},
  };
  if (deps.news) for (const p of closeCalls(facts)) facts.news[p.id] = await deps.news(p, at);
  return facts;
}

/** Players whose claim or drop is close: the top free agent per position and any within CLOSE_GAP of it, every widely-owned one, and the weakest bench players. */
export function closeCalls(f: WaiverFacts): WPlayer[] {
  const out = new Set<WPlayer>(f.widely.map((id) => f.free[id]));
  for (const ids of Object.values(f.buckets)) {
    const top = f.free[ids[0]];
    if (top) ids.map((id) => f.free[id]).filter((p) => top.proj - p.proj < CLOSE_GAP).forEach((p) => out.add(p));
  }
  const bench = Object.values(f.roster).filter((p) => p.role === 'bench').sort((a, b) => a.proj - b.proj);
  bench.slice(0, 2).forEach((p) => out.add(p));
  return [...out];
}
