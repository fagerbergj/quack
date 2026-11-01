import type { Json, SleeperClient } from '../client.ts';
import { ownerTeam, ptsKey, type LineupInput, type NewsItem } from '../lineup/facts.ts';
import type { Searchable } from '../lineup/news.ts';
import { fits, lineupSlots } from '../lineup/slots.ts';
import { byeWeeks, COVER, POSITIONS, teamNeeds, UNAVAILABLE, wplayer, type Bye, type WPlayer } from '../waivers/facts.ts';

export type TPlayer = WPlayer & { role: 'starter' | 'bench' | 'ir' };
export type Stance = 'contender' | 'rebuilder';
/** One team as the trade jobs see it: standing, what it lacks and what it can spare. `surplus` is player ids. */
export type Side = {
  rosterId: number;
  team: string;
  owner: string;
  record: string;
  rank: number;
  stance: Stance;
  needs: Record<string, number>;
  byes: Bye[];
  surplus: string[];
  players: Record<string, TPlayer>;
};
/** From this team's side, whoever proposed it. */
export type Offer = { partner: number; give: string[]; get: string[] };

/** What the trade jobs computed in code and the model reasons over; the rubric's gates check the card against it. */
export type TradeFacts = {
  league: string;
  week: number;
  team: string;
  teams: number;
  playoffTeams: number;
  mine: Side;
  /** Every other team for the finder; the one partner for an analysis. */
  partners: Side[];
  offer: Offer | null;
};

export type TradeInput = LineupInput & { partner: string; give: string[]; get: string[] };
export type FinderInput = LineupInput;
export type TradeDeps = { client: SleeperClient };
export type NewsSearch = (p: Searchable, at: number) => Promise<NewsItem[]>;

// Kickers and defenses are streamed, not traded.
const TRADEABLE = POSITIONS.filter((p) => p !== 'K' && p !== 'DEF');
const byProj = (a: WPlayer, b: WPlayer) => b.proj - a.proj || a.id.localeCompare(b.id);

/** Healthy players left once each position keeps its starting slots plus one skill-position cover, best projection first. */
function surplusOf(players: WPlayer[], slots: string[]): string[] {
  const healthy = players.filter((p) => !UNAVAILABLE.has(p.inj ?? ''));
  return TRADEABLE.flatMap((pos) => {
    const keep = slots.filter((s) => s === pos).length + (COVER[pos] ?? 0);
    return healthy.filter((p) => p.pos === pos).sort(byProj).slice(keep).map((p) => p.id);
  });
}

const points = (r: Json) => (r.settings?.fpts ?? 0) + (r.settings?.fpts_decimal ?? 0) / 100;
const recordOf = (r: Json) => `${r.settings?.wins ?? 0}-${r.settings?.losses ?? 0}${r.settings?.ties ? `-${r.settings.ties}` : ''}`;

/** Roster id to 1-based standing: wins, then points for. */
function standings(rosters: Json[]): Map<number, number> {
  const order = [...rosters].sort((a, b) => (b.settings?.wins ?? 0) - (a.settings?.wins ?? 0) || points(b) - points(a) || a.roster_id - b.roster_id);
  return new Map(order.map((r, i) => [r.roster_id, i + 1]));
}

type Ctx = { users: Json[]; slots: string[]; positions: string[]; week: number; ranks: Map<number, number>; playoffTeams: number; dump: Json; wp: (id: string) => WPlayer };

function sideOf(r: Json, c: Ctx): Side {
  const user = c.users.find((u) => u.user_id === r.owner_id);
  const off = new Set<string>([...(r.reserve ?? []), ...(r.taxi ?? [])]);
  const starters = new Set<string>(r.starters ?? []);
  const ids: string[] = (r.players ?? []).filter((id: string) => c.dump[id]);
  const players = Object.fromEntries(ids.map((id) => [id, { ...c.wp(id), role: off.has(id) ? 'ir' : starters.has(id) ? 'starter' : 'bench' } as TPlayer]));
  const active = Object.values(players).filter((p) => p.role !== 'ir');
  const rank = c.ranks.get(r.roster_id)!;
  return {
    rosterId: r.roster_id,
    team: user?.metadata?.team_name ?? user?.display_name ?? `roster ${r.roster_id}`,
    owner: user?.display_name ?? `roster ${r.roster_id}`,
    record: recordOf(r),
    rank,
    stance: rank <= c.playoffTeams ? 'contender' : 'rebuilder',
    ...teamNeeds(active, c.slots, c.positions, c.week),
    surplus: surplusOf(active, c.slots),
    players,
  };
}

/** The partner by roster id, user id, display name or team name; anything else, or yourself, is an error. */
function findPartner(sides: Side[], mineId: number, ref: string): Side {
  const hit = sides.find((s) => [String(s.rosterId), s.owner, s.team].includes(ref));
  if (!hit || hit.rosterId === mineId) throw new Error(`sleeper: no partner team "${ref}" in the league`);
  return hit;
}

function checkOffer(mine: Side, partner: Side, give: string[], get: string[]): void {
  if (!give.length || !get.length) throw new Error('sleeper: a trade needs players on both sides');
  for (const id of give) if (!mine.players[id]) throw new Error(`sleeper: ${id} is not on your roster`);
  for (const id of get) if (!partner.players[id]) throw new Error(`sleeper: ${id} is not on ${partner.team}`);
}

export async function buildTradeFacts(deps: TradeDeps, snap: Json, input: FinderInput | TradeInput): Promise<TradeFacts> {
  const { league, users, rosters } = snap.data;
  const { user, roster } = ownerTeam(snap, input.owner);
  const { client } = deps;
  const [dump, proj, schedule] = await Promise.all([client.players(), client.projections(league.season, input.week), client.schedule(league.season)]);
  const byes = byeWeeks(schedule);
  const key = ptsKey(league.scoring_settings?.rec);
  const slots = lineupSlots(league.roster_positions);
  const playoffTeams = league.settings?.playoff_teams ?? Math.ceil(rosters.length / 2);
  const ctx: Ctx = { users, slots, positions: POSITIONS.filter((p) => slots.some((s) => fits(s, p))), week: input.week, ranks: standings(rosters), playoffTeams, dump, wp: (id) => wplayer(id, dump, proj, key, byes) };
  const sides: Side[] = rosters.map((r: Json) => sideOf(r, ctx));
  const mine = sides.find((s) => s.rosterId === roster.roster_id)!;
  const others = sides.filter((s) => s !== mine);
  const facts = { league: input.league, week: input.week, team: user.metadata?.team_name ?? user.display_name, teams: rosters.length, playoffTeams, mine };
  if (!('partner' in input)) return { ...facts, partners: others, offer: null };
  const partner = findPartner(sides, mine.rosterId, input.partner);
  checkOffer(mine, partner, input.give, input.get);
  return { ...facts, partners: [partner], offer: { partner: partner.rosterId, give: input.give, get: input.get } };
}

/** Looks a player up on any team in the facts; ids are unique across rosters. */
export const playerOf = (f: TradeFacts, id: string): TPlayer | undefined => [f.mine, ...f.partners].map((s) => s.players[id]).find(Boolean);

/** Searches news for each player once; a missing source means no links, not an error. */
export async function fetchNews(search: NewsSearch | undefined, players: TPlayer[], at: number): Promise<Record<string, NewsItem[]>> {
  const out: Record<string, NewsItem[]> = {};
  if (search) for (const p of players) out[p.id] ??= await search(p, at);
  return out;
}
