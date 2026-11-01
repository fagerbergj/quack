import type { NewsItem } from '../lineup/facts.ts';
import type { PlayerRef } from '../lineup/card.ts';
import type { FreeAgent, Mine, WaiverFacts, WaiverType, WPlayer } from './facts.ts';

/** Sized in the league's own currency: a share of remaining FAAB, or the queue position that is spent. */
export type Size = { kind: 'faab'; bid: number; left: number; pct: number } | { kind: 'priority'; position: number | null; of: number };
export type Candidate = {
  rank: number;
  player: PlayerRef;
  proj: number;
  owned_pct: number | null;
  adds_24h: number;
  bye: number | null;
  drop: { player: PlayerRef; role: Mine['role'] };
  size: Size;
  why: string;
  /** Attached by code from the facts, for the add and the drop, so a link is never the model's to copy. */
  news: Array<NewsItem & { player: string }>;
};
export type WaiversCard = {
  kind: 'waivers';
  league: string;
  week: number;
  team: string;
  waiver_type: WaiverType;
  my_priority: number | null;
  faab_left: number | null;
  teams: number;
  summary: string;
  candidates: Candidate[];
  also_checked: Array<{ player: PlayerRef; why: string }>;
};

/** What the model decides (SL-6: no predictions in it); ids and a bid, never figures to copy. */
export type WaiverDecision = { claims: Array<{ add: string; drop: string; bid?: number | null; why: string }>; passed: Array<{ playerId: string; why: string }>; summary: string };

const ref = (p: WPlayer): PlayerRef => ({ id: p.id, name: p.name, pos: p.pos, team: p.team, inj: p.inj });

function sizeOf(f: WaiverFacts, bid: number | null | undefined, add: string): Size {
  if (f.waiverType !== 'faab') return { kind: 'priority', position: f.myPriority, of: f.teams };
  const left = f.faabLeft ?? 0;
  if (!Number.isInteger(bid) || bid! < 0 || bid! > left) throw new Error(`the model bid ${bid} for ${add} in a FAAB league with ${left} left`);
  return { kind: 'faab', bid: bid!, left, pct: left ? Math.round((bid! / left) * 100) : 0 };
}

/** Figures come from the facts; the model supplies the order, the drops, a bid and the reasons. */
export function buildWaiversCard(f: WaiverFacts, d: WaiverDecision): WaiversCard {
  const adds = d.claims.map((c) => c.add);
  const dup = adds.filter((id, i) => adds.indexOf(id) !== i);
  if (dup.length) throw new Error(`the model claimed ${dup.join(', ')} more than once`);
  const candidates = d.claims.map((c, i): Candidate => {
    const add: FreeAgent | undefined = f.free[c.add];
    const drop: Mine | undefined = f.roster[c.drop];
    if (!add) throw new Error(`the model claimed ${c.add}, who is not a free agent`);
    if (!drop) throw new Error(`the model dropped ${c.drop}, who is not on the roster`);
    const news = [add, drop].flatMap((p) => (f.news[p.id] ?? []).map((n) => ({ ...n, player: p.name })));
    return { rank: i + 1, player: ref(add), proj: add.proj, owned_pct: add.owned, adds_24h: add.adds, bye: add.bye, drop: { player: ref(drop), role: drop.role }, size: sizeOf(f, c.bid, c.add), why: c.why, news };
  });
  const seen = new Set(adds);
  const also_checked = d.passed.filter((p) => f.free[p.playerId] && !seen.has(p.playerId)).map((p) => ({ player: ref(f.free[p.playerId]), why: p.why }));
  const summary = candidates.length || /no claim/i.test(d.summary) ? d.summary : 'No claims: nothing on the wire beats the roster.';
  return { kind: 'waivers', league: f.league, week: f.week, team: f.team, waiver_type: f.waiverType, my_priority: f.myPriority, faab_left: f.faabLeft, teams: f.teams, summary, candidates, also_checked };
}
