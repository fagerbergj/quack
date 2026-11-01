import type { Json, SleeperClient } from '../client.ts';
import { ownerTeam, round2, type FactsDeps, type LineupInput, type NewsItem } from '../lineup/facts.ts';
import { playerName, record, remainingWeeks, standings, teamNames, teamOf, weekPhase, type Standing } from './week.ts';

const UNAVAILABLE = new Set(['Out', 'IR', 'Doubtful', 'Sus']);
/** Finished weeks a preview averages over. */
const RECENT_WEEKS = 3;
const TOP = 2;

export type Scorer = { id: string; name: string; pts: number };
/** `pts` is the final score in a recap and the code-computed lineup_avg in a preview; `out` names starters counted 0. */
export type DigestSide = { rosterId: number; team: string; record: string; pts: number; top: Scorer[]; out: string[]; designated: Array<{ id: string; name: string; inj: string }> };
export type DigestGame = { id: number; sides: [DigestSide, DigestSide]; margin: number; mine: boolean };
export type DigestFacts = {
  league: string;
  week: number;
  mode: 'preview' | 'recap';
  games: DigestGame[];
  standings: Standing[];
  playoffTeams: number;
  remainingWeeks: number;
  sourceNote: string;
  news: Record<string, Array<NewsItem & { player: string }>>;
};

/** Each player's points by week across every roster, newest finished week first. */
async function recentPoints(client: SleeperClient, league: string, latest: number): Promise<Array<Map<string, number>>> {
  const weeks = Array.from({ length: RECENT_WEEKS }, (_, i) => latest - i).filter((w) => w >= 1);
  const all = await Promise.all(weeks.map((w) => client.matchups(league, w)));
  return all.map((ms) => new Map<string, number>(ms.flatMap((m: Json) => Object.entries<number>(m.players_points ?? {}))));
}

type Ctx = { dump: Json; playing: Set<string>; recent: Array<Map<string, number>>; mode: DigestFacts['mode'] };

function side(ctx: Ctx, m: Json, names: Map<number, string>, rosters: Json[]): DigestSide {
  const ids: string[] = (m.starters ?? []).filter((id: string) => id !== '0');
  const rows = ids.map((id) => {
    const inj: string | null = ctx.dump[id]?.injury_status ?? null;
    const team = teamOf(ctx.dump, id);
    if (ctx.mode === 'recap') return { id, name: playerName(ctx.dump, id), pts: m.players_points?.[id] ?? 0, out: null as string | null, inj };
    const seen = ctx.recent.map((w) => w.get(id)).filter((v): v is number => v !== undefined);
    const out = !team || !ctx.playing.has(team) ? 'bye' : UNAVAILABLE.has(inj ?? '') ? inj : null;
    return { id, name: playerName(ctx.dump, id), pts: out || !seen.length ? 0 : seen.reduce((a, b) => a + b, 0) / seen.length, out, inj };
  });
  const pts = ctx.mode === 'recap' ? round2(m.points ?? 0) : round2(rows.reduce((n, r) => n + r.pts, 0));
  const top = rows.filter((r) => !r.out && (ctx.mode === 'recap' || r.pts > 0)).sort((a, b) => b.pts - a.pts).slice(0, TOP).map((r) => ({ id: r.id, name: r.name, pts: round2(r.pts) }));
  return {
    rosterId: m.roster_id,
    team: names.get(m.roster_id)!,
    record: record(rosters.find((r) => r.roster_id === m.roster_id)!),
    pts,
    top,
    out: rows.filter((r) => r.out).map((r) => `${r.name} (${r.out})`),
    designated: rows.filter((r) => r.inj && !r.out).map((r) => ({ id: r.id, name: r.name, inj: r.inj! })),
  };
}

function sourceNote(mode: DigestFacts['mode'], week: number, recentCount: number, sides: DigestSide[]): string {
  if (mode === 'recap') return `Final scores from Sleeper, week ${week}.`;
  const zero = sides.flatMap((s) => s.out);
  if (!recentCount) return 'No finished weeks yet, so every side shows 0; the news carries the preview.';
  return `Points are each side's starters' average over the last ${recentCount} finished week${recentCount > 1 ? 's' : ''}; counted 0: ${zero.join(', ') || 'nobody'}.`;
}

/** Every game's numbers, computed here; the model never sees a projection, only code-built figures (audit contradiction 1). */
export async function buildDigestFacts(deps: FactsDeps, snap: Json, input: LineupInput, at: number): Promise<DigestFacts> {
  const { league, users, rosters, state } = snap.data;
  const { client } = deps;
  const [matchups, schedule, dump] = await Promise.all([client.matchups(input.league, input.week), client.schedule(league.season), client.players()]);
  const phase = weekPhase(schedule, input.week, state.week);
  if (phase === 'live') throw new Error(`sleeper: week ${input.week} is in progress; the digest runs before its first kickoff or once its last game is final`);
  const mode = phase;
  const recent = mode === 'preview' ? await recentPoints(client, input.league, Math.min(input.week, state.week) - 1) : [];
  const ctx: Ctx = { dump, mode, recent, playing: new Set(schedule.filter((g: Json) => g.week === input.week).flatMap((g: Json) => [g.home, g.away])) };
  const names = teamNames(users, rosters);
  const mine = ownerTeam(snap, input.owner).roster.roster_id;
  const groups = Map.groupBy(matchups.filter((m: Json) => m.matchup_id != null), (m: Json) => m.matchup_id as number);
  const games = [...groups].flatMap(([id, ms]): DigestGame[] => {
    if (ms.length !== 2) return [];
    const sides = ms.sort((a: Json, b: Json) => a.roster_id - b.roster_id).map((m: Json) => side(ctx, m, names, rosters)) as [DigestSide, DigestSide];
    return [{ id, sides, margin: round2(Math.abs(sides[0].pts - sides[1].pts)), mine: sides.some((s) => s.rosterId === mine) }];
  });
  const table = standings(league, rosters, names);
  const news: DigestFacts['news'] = {};
  if (deps.news && mode === 'preview') {
    for (const s of games.flatMap((g) => g.sides)) {
      const wanted = [...s.top, ...s.designated].map((p) => p.id);
      for (const id of new Set<string>(wanted)) news[id] = (await deps.news({ name: playerName(dump, id), team: teamOf(dump, id), pos: dump[id]?.position ?? '' }, at)).map((n) => ({ ...n, player: playerName(dump, id) }));
    }
  }
  return { league: input.league, week: input.week, mode, games, standings: table.rows, playoffTeams: table.playoffTeams, remainingWeeks: remainingWeeks(league, state.week), sourceNote: sourceNote(mode, input.week, recent.length, games.flatMap((g) => g.sides)), news };
}

export type DigestCard = {
  kind: 'digest';
  league: string;
  week: number;
  past: boolean;
  summary: string;
  games: Array<{
    id: number;
    home: { team: string; pts: number; top_scorers: Array<{ name: string; pts: number }> };
    away: { team: string; pts: number; top_scorers: Array<{ name: string; pts: number }> };
    margin: number;
    mine: boolean;
    note: string;
    /** Attached by code for the game's top scorers and designated starters, so a link is never the model's to copy. */
    news: Array<NewsItem & { player: string }>;
  }>;
  playoff_line: { teams: number; remaining_weeks: number; standings: Standing[] };
  source_note: string;
};

/** What the model decides: the story and one note per game (SL-6: no projections, no win probabilities). */
export type DigestDecision = { summary: string; notes: Array<{ matchup: number; note: string }> };

/** Sleeper has no home side; the lower roster id is listed first. */
export function buildDigestCard(f: DigestFacts, d: DigestDecision): DigestCard {
  const ids = new Set(f.games.map((g) => g.id));
  const bad = d.notes.filter((n) => !ids.has(n.matchup));
  if (bad.length) throw new Error(`the model wrote notes for matchups that are not in week ${f.week}: ${bad.map((n) => n.matchup).join(', ')}`);
  const out = (s: DigestSide) => ({ team: s.team, pts: s.pts, top_scorers: s.top.map((t) => ({ name: t.name, pts: t.pts })) });
  const games = f.games.map((g) => ({
    id: g.id,
    home: out(g.sides[0]),
    away: out(g.sides[1]),
    margin: g.margin,
    mine: g.mine,
    note: d.notes.find((n) => n.matchup === g.id)?.note ?? '',
    news: g.sides.flatMap((s) => [...s.top, ...s.designated].flatMap((p) => f.news[p.id] ?? [])),
  }));
  return { kind: 'digest', league: f.league, week: f.week, past: f.mode === 'recap', summary: d.summary, games, playoff_line: { teams: f.playoffTeams, remaining_weeks: f.remainingWeeks, standings: f.standings }, source_note: f.sourceNote };
}
