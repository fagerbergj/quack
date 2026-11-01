import type { Json } from '../client.ts';
import { ownerTeam, ptsKey, round2, type FactsDeps, type LineupInput, type NewsItem } from '../lineup/facts.ts';
import { bestLineup, fits, lineupSlots, numberSlots } from '../lineup/slots.ts';
import { nextProcessing } from '../waivers/jobs.ts';
import { kickoffs, playerName, teamNames, teamOf, weekPhase } from './week.ts';

const FANTASY = new Set(['QB', 'RB', 'WR', 'TE', 'K', 'DEF']);
const MAX_WAIVER_MISSES = 3;
const DAY = 86_400_000;

export type Scored = { id: string; name: string; pos: string; pts: number };
type Signalled = NewsItem & { player: string };
/** `before` is the instant a signal must precede: the player's kickoff, or the waiver run for a waiver miss. */
export type Miss = { slot: string; started: Scored; better: Scored; swing: number; before: string; news: Signalled[] };
export type WaiverMiss = { player: Scored & { team: string | null }; over: { slot: string; name: string; pts: number }; before: string; news: Signalled[] };
export type RetroFacts = { league: string; week: number; team: string; opponent: string | null; started: number; opp: number | null; won: boolean; best: number; left: number; misses: Miss[]; waiverMisses: WaiverMiss[] };

type Slot = { slot: string; id: string | null; pts: number };

/** Pairs each best-lineup player who did not start with the lowest-scoring unpaired starter he could legally have replaced. */
function pairSwaps(ins: Scored[], outs: Slot[]): Array<{ slot: Slot; better: Scored }> {
  const used = new Set<Slot>();
  return ins.flatMap((better) => {
    const slot = outs.filter((o) => !used.has(o) && fits(o.slot, better.pos)).sort((a, b) => a.pts - b.pts)[0];
    return slot ? (used.add(slot), [{ slot, better }]) : [];
  });
}

/** The last waiver run at or before an instant, so a waiver miss is judged on what was knowable then. */
function lastProcessing(settings: Json, by: number): number | null {
  let last: number | null = null;
  for (let n = nextProcessing(by - 8 * DAY, settings ?? {}); n !== null && n <= by; n = nextProcessing(n, settings ?? {})) last = n;
  return last;
}

function freeAgentHits(starters: Slot[], stats: Json, rostered: Set<string>, dump: Json, key: string): Array<Scored & { team: string | null; over: Slot }> {
  const hits = Object.entries<Json>(stats).flatMap(([id, s]) => {
    const pos = dump[id]?.position;
    if (rostered.has(id) || !FANTASY.has(pos)) return [];
    const weakest = starters.filter((o) => o.id && fits(o.slot, pos)).sort((a, b) => a.pts - b.pts)[0];
    const pts = s?.[key] ?? 0;
    return weakest && pts > weakest.pts ? [{ id, name: playerName(dump, id), pos, pts, team: teamOf(dump, id), over: weakest }] : [];
  });
  return hits.sort((a, b) => b.pts - b.over.pts - (a.pts - a.over.pts) || b.pts - a.pts || a.name.localeCompare(b.name)).slice(0, MAX_WAIVER_MISSES);
}

/** Every figure of the retro, computed here from the final matchup: the model only explains them. */
export async function buildRetroFacts(deps: FactsDeps, snap: Json, input: LineupInput): Promise<RetroFacts> {
  const { league, users, rosters, state } = snap.data;
  const { client } = deps;
  const { roster } = ownerTeam(snap, input.owner);
  const schedule = await client.schedule(league.season);
  if (weekPhase(schedule, input.week, state.week) !== 'recap') throw new Error(`sleeper: week ${input.week} is not final; the retro runs after its last game`);
  const [matchups, dump, scores, stats] = await Promise.all([client.matchups(input.league, input.week), client.players(), client.scores(league.season, input.week), client.stats(league.season, input.week)]);
  const mine = matchups.find((m: Json) => m.roster_id === roster.roster_id);
  if (!mine) throw new Error(`sleeper: no week ${input.week} matchup for roster ${roster.roster_id}`);
  const theirs = matchups.find((m: Json) => m.matchup_id === mine.matchup_id && m.roster_id !== mine.roster_id);
  const key = ptsKey(league.scoring_settings?.rec);
  const slotNames = numberSlots(lineupSlots(league.roster_positions));
  const pts: Record<string, number> = mine.players_points ?? {};
  const scored = (id: string): Scored => ({ id, name: playerName(dump, id), pos: dump[id]?.position ?? '', pts: pts[id] ?? 0 });
  const started: Slot[] = slotNames.map((slot, i) => ({ slot, id: mine.starters[i] && mine.starters[i] !== '0' ? (mine.starters[i] as string) : null, pts: pts[mine.starters[i]] ?? 0 }));
  const bestIds = bestLineup(slotNames, mine.players.filter((id: string) => id !== '0').map((id: string) => ({ id, pos: dump[id]?.position ?? '', pts: pts[id] ?? 0 }))).filter((id): id is string => id !== null);
  const startedIds = new Set(started.map((s) => s.id));
  const ins = bestIds.filter((id) => !startedIds.has(id)).map(scored).sort((a, b) => b.pts - a.pts);
  const outs = started.filter((s) => s.id && !bestIds.includes(s.id));
  const best = round2(bestIds.reduce((n, id) => n + (pts[id] ?? 0), 0));

  const kick = kickoffs(schedule, scores, input.week);
  const first = Math.min(...kick.values());
  const waiverRun = lastProcessing(league.settings, first) ?? first;
  const search = async (before: number, ...players: Scored[]): Promise<Signalled[]> => {
    if (!deps.news) return [];
    const found = await Promise.all(players.map(async (p) => (await deps.news!({ name: p.name, team: teamOf(dump, p.id), pos: p.pos }, before)).map((n) => ({ ...n, player: p.name }))));
    return found.flat();
  };
  const misses = await Promise.all(pairSwaps(ins, outs).map(async ({ slot, better }): Promise<Miss> => {
    const out = scored(slot.id!);
    const before = Math.min(kick.get(teamOf(dump, better.id) ?? '') ?? first, kick.get(teamOf(dump, out.id) ?? '') ?? first);
    return { slot: slot.slot, started: out, better, swing: round2(better.pts - out.pts), before: new Date(before).toISOString(), news: await search(before, better, out) };
  }));
  const rostered = new Set<string>(matchups.flatMap((m: Json) => m.players ?? []));
  const waiverMisses = await Promise.all(freeAgentHits(started, stats, rostered, dump, key).map(async (h): Promise<WaiverMiss> => ({
    player: { id: h.id, name: h.name, pos: h.pos, pts: h.pts, team: h.team },
    over: { slot: h.over.slot, name: playerName(dump, h.over.id!), pts: h.over.pts },
    before: new Date(waiverRun).toISOString(),
    news: await search(waiverRun, h),
  })));
  return { league: input.league, week: input.week, team: teamNames(users, rosters).get(roster.roster_id)!, opponent: theirs ? (teamNames(users, rosters).get(theirs.roster_id) ?? null) : null, started: round2(mine.points ?? 0), opp: theirs ? round2(theirs.points ?? 0) : null, won: !!theirs && mine.points > theirs.points, best, left: round2(best - (mine.points ?? 0)), misses, waiverMisses };
}

export type RetroCard = {
  kind: 'retro';
  league: string;
  week: number;
  team: string;
  opponent: string | null;
  started: number;
  opp: number | null;
  won: boolean;
  best: number;
  left: number;
  misses: Array<{ slot: string; started: { name: string; pts: number }; better: { name: string; pts: number }; swing: number; knowable: boolean; signal: Signalled | null; before: string; why: string }>;
  waiver_misses: Array<{ name: string; pos: string; pts: number; over_name: string; over_pts: number; knowable: boolean; signal: Signalled | null; before: string; why: string }>;
  lessons: string[];
  summary: string;
};

type RowCall = { index: number; why: string; knowable: boolean; signalUrl?: string };
/** What the model decides: the story behind each row and the process fixes; every figure stays the facts'. */
export type RetroDecision = { misses: RowCall[]; waiverMisses: RowCall[]; lessons: string[]; summary: string };

const NONE = 'No pre-kickoff signal was found.';

/** A row is knowable only when the model cites a link code attached, dated before the row's cutoff. */
function judged(rows: Array<{ news: Signalled[]; before: string }>, calls: RowCall[], label: string) {
  const bad = calls.filter((c) => !rows[c.index]);
  if (bad.length) throw new Error(`the model graded ${label} ${bad.map((c) => c.index).join(', ')}, which do not exist`);
  return rows.map((r, i) => {
    const call = calls.find((c) => c.index === i);
    const signal = (call?.knowable && r.news.find((n) => n.url === call.signalUrl && Date.parse(n.date) < Date.parse(r.before))) || null;
    return { knowable: signal !== null, signal, before: r.before, why: call?.why ?? NONE };
  });
}

export function buildRetroCard(f: RetroFacts, d: RetroDecision): RetroCard {
  const m = judged(f.misses, d.misses, 'miss');
  const w = judged(f.waiverMisses, d.waiverMisses, 'waiver miss');
  return {
    kind: 'retro', league: f.league, week: f.week, team: f.team, opponent: f.opponent, started: f.started, opp: f.opp, won: f.won, best: f.best, left: f.left,
    misses: f.misses.map((x, i) => ({ slot: x.slot, started: { name: x.started.name, pts: x.started.pts }, better: { name: x.better.name, pts: x.better.pts }, swing: x.swing, ...m[i] })),
    waiver_misses: f.waiverMisses.map((x, i) => ({ name: x.player.name, pos: x.player.pos, pts: x.player.pts, over_name: x.over.name, over_pts: x.over.pts, ...w[i] })),
    lessons: d.lessons.slice(0, 3),
    summary: d.summary,
  };
}
