import type { Json, SleeperClient } from '../client.ts';
import type { Searchable } from './news.ts';
import { bestLineup, fits, lineupSlots, numberSlots } from './slots.ts';

export type Game = { opponent: string; home: boolean; kickoff: string | null; locked: boolean; status: string | null };
export type PlayerFact = { id: string; name: string; pos: string; team: string | null; inj: string | null; proj: number; game: Game | null };
/** `date` is the report's own publication time (ISO), never the fetch time. */
export type NewsItem = { title: string; url: string; date: string };

/** What the lineup job computed in code and the model reasons over; the rubric's gates check the card against it. */
export type Facts = {
  league: string;
  week: number;
  team: string;
  record: string;
  opponent: string | null;
  /** Numbered slots with a player or a candidate, in starters order. */
  slots: string[];
  /** The player now in each slot (Sleeper's own lineup), null when empty. */
  current: Record<string, string | null>;
  /** Slots whose incumbent's game has not locked: the only ones that may change. */
  open: string[];
  players: Record<string, PlayerFact>;
  /** Per open slot, unlocked legal players best first. */
  candidates: Record<string, string[]>;
  baseline: Array<{ slot: string; id: string; name: string; pos: string; proj: number }>;
  oppProj: number | null;
  news: Record<string, NewsItem[]>;
};

export type LineupInput = { league: string; week: number; owner: string };
export type FactsDeps = { client: SleeperClient; news?: (p: Searchable, at: number) => Promise<NewsItem[]> };

// Out, IR, Doubtful and suspended players are not lineup candidates; Questionable ones are, and the model weighs them.
const UNAVAILABLE = new Set(['Out', 'IR', 'Doubtful', 'Sus']);
/** A bench player this close to a starter's projection makes the slot a close call. */
export const CLOSE_GAP = 3;

export const ptsKey = (rec: unknown) => (rec === 1 ? 'pts_ppr' : rec === 0.5 ? 'pts_half_ppr' : 'pts_std');
export const round2 = (n: number) => Math.round(n * 100) / 100;

/** One week's games by NFL team, joined on game_id so a missing score leaves the kickoff unknown rather than borrowing another game's. */
function gamesByTeam(schedule: Json[], scores: Json[], week: number, at: number): Map<string, Game> {
  const starts = new Map<string, number>(scores.filter((s) => s.start_time > 0 && !['flex-schedule', 'postponed', 'cancelled'].includes(s.metadata?.status)).map((s) => [s.game_id, s.start_time]));
  const out = new Map<string, Game>();
  for (const g of schedule.filter((x) => x.week === week && x.status !== 'canceled' && x.status !== 'postponed')) {
    const start = starts.get(g.game_id);
    const locked = g.status === 'in_game' || g.status === 'complete' || (start !== undefined && start <= at);
    const kickoff = start === undefined ? null : new Date(start).toISOString();
    out.set(g.home, { opponent: g.away, home: true, kickoff, locked, status: g.status ?? null });
    out.set(g.away, { opponent: g.home, home: false, kickoff, locked, status: g.status ?? null });
  }
  return out;
}

function playerFact(id: string, dump: Json, proj: Json, key: string, games: Map<string, Game>): PlayerFact {
  const p = dump[id] ?? {};
  const pos = p.position ?? '';
  const team: string | null = p.team ?? (pos === 'DEF' ? id : null);
  return {
    id,
    name: [p.first_name, p.last_name].filter(Boolean).join(' ') || id,
    pos,
    team,
    inj: p.injury_status ?? null,
    proj: round2(proj[id]?.[key] ?? 0),
    game: (team && games.get(team)) || null,
  };
}

const usable = (p: PlayerFact) => !UNAVAILABLE.has(p.inj ?? '') && p.game !== null && !p.game.locked;

export function ownerTeam(snap: Json, owner: string) {
  const { users, rosters } = snap.data;
  const user = users.find((u: Json) => [u.user_id, u.display_name, u.username].includes(owner));
  const roster = user && rosters.find((r: Json) => r.owner_id === user.user_id);
  if (!roster) throw new Error(`sleeper: no team for ${owner}`);
  return { user, roster };
}

/** The slots this team fills, who is in them now, and which may still change. */
function slotState(slotNames: string[], mine: Json, players: Record<string, PlayerFact>) {
  const current = Object.fromEntries(slotNames.map((s, i) => [s, players[mine.starters[i]] ? (mine.starters[i] as string) : null]));
  const open = slotNames.filter((s) => !current[s] || !players[current[s]!].game?.locked);
  const ids = Object.keys(players);
  const candidates = Object.fromEntries(open.map((s) => [s, ids.filter((id) => fits(s, players[id].pos) && usable(players[id])).sort((a, b) => players[b].proj - players[a].proj)]));
  // With nobody better, an unavailable incumbent still stays in an open slot.
  for (const s of open) if (!candidates[s].length && current[s]) candidates[s] = [current[s]!];
  return { current, open, candidates };
}

export async function buildFacts(deps: FactsDeps, snap: Json, input: LineupInput, at: number): Promise<Facts> {
  const { league, users, rosters } = snap.data;
  const { user, roster } = ownerTeam(snap, input.owner);
  const { client } = deps;
  const [matchups, dump, proj, schedule, scores] = await Promise.all([
    client.matchups(input.league, input.week), client.players(), client.projections(league.season, input.week), client.schedule(league.season), client.scores(league.season, input.week),
  ]);
  const mine = matchups.find((m: Json) => m.roster_id === roster.roster_id);
  if (!mine) throw new Error(`sleeper: no week ${input.week} matchup for roster ${roster.roster_id}`);
  const theirs = matchups.find((m: Json) => m.matchup_id === mine.matchup_id && m.roster_id !== mine.roster_id);
  const games = gamesByTeam(schedule, scores, input.week, at);
  const key = ptsKey(league.scoring_settings?.rec);

  const slotNames = numberSlots(lineupSlots(league.roster_positions));
  const benched = new Set<string>([...(roster.reserve ?? []), ...(roster.taxi ?? [])]);
  const ids: string[] = mine.players.filter((id: string) => id !== '0' && !benched.has(id));
  const players = Object.fromEntries(ids.map((id) => [id, playerFact(id, dump, proj, key, games)]));
  const { current, open, candidates } = slotState(slotNames, mine, players);

  const eligible = Object.values(players).filter((p) => !UNAVAILABLE.has(p.inj ?? ''));
  const baseline = bestLineup(slotNames, eligible.map((p) => ({ id: p.id, pos: p.pos, pts: p.proj }))).flatMap((id, i) => (id ? [{ slot: slotNames[i], id, name: players[id].name, pos: players[id].pos, proj: players[id].proj }] : []));
  const opponentRoster = rosters.find((r: Json) => r.roster_id === theirs?.roster_id);
  const record = (r: Json) => `${r?.settings?.wins ?? 0}-${r?.settings?.losses ?? 0}`;
  const facts: Facts = {
    league: input.league,
    week: input.week,
    team: user.metadata?.team_name ?? user.display_name,
    record: record(roster),
    opponent: users.find((u: Json) => u.user_id === opponentRoster?.owner_id)?.display_name ?? null,
    slots: slotNames.filter((s) => current[s] || candidates[s]?.length),
    current,
    open,
    players,
    candidates,
    baseline,
    oppProj: theirs ? round2((theirs.starters as string[]).reduce((n, id) => n + (proj[id]?.[key] ?? 0), 0)) : null,
    news: {},
  };
  if (deps.news) for (const p of closeCalls(facts)) facts.news[p.id] = await deps.news(p, at);
  return facts;
}

/** Players whose status or projection gap makes the call close: Questionable, or within CLOSE_GAP of the incumbent. */
export function closeCalls(f: Facts): PlayerFact[] {
  const out = new Set<PlayerFact>();
  for (const s of f.open) {
    const inc = f.current[s] ? f.players[f.current[s]!] : undefined;
    for (const id of f.candidates[s]) {
      const p = f.players[id];
      if (p.inj === 'Questionable' || (inc && id !== inc.id && Math.abs(p.proj - inc.proj) < CLOSE_GAP)) out.add(p);
    }
    if (inc?.inj === 'Questionable') out.add(inc);
  }
  return [...out];
}
