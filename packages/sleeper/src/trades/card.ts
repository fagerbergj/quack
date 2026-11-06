import type { PlayerRef } from '../lineup/card.ts';
import type { NewsItem } from '../lineup/facts.ts';
import type { Side, Stance, TPlayer, TradeFacts } from './facts.ts';

export const VERDICTS = ['send', 'counter', 'decline'] as const;
export type Verdict = (typeof VERDICTS)[number];
export const MAX_SUGGESTIONS = 3;

export type TRef = PlayerRef & { proj: number; bye: number | null };
type Linked = Array<NewsItem & { player: string }>;
export type PartnerRef = { rosterId: number; team: string; record: string; stance: Stance; needs: Record<string, number> };
/** Projection swing this week, per side: what each receives minus what each sends. */
export type Delta = { you: number; partner: number };
export type Offered = { give: TRef[]; get: TRef[]; delta: Delta };

export type TradeCard = {
  kind: 'trade';
  league: string;
  week: number;
  team: string;
  summary: string;
  partner: PartnerRef;
  /** The proposed trade first; a counter, when the verdict is one, rides on it. */
  offers: Array<Offered & { verdict: Verdict; why: string; flips_if: string; counter: Offered | null; news: Linked }>;
};
export type FinderCard = {
  kind: 'trade-finder';
  league: string;
  week: number;
  team: string;
  summary: string;
  suggestions: Array<{ rank: number; partner: PartnerRef; partner_need: string; give: TRef; get: TRef; delta: Delta; note: string; news: Linked }>;
};

/** What the model decides: ids and prose, never figures to copy. */
export type TradeDecision = { verdict: Verdict; why: string; flips_if: string; counter?: { give: string[]; get: string[] } | null; summary: string };
export type FinderDecision = { suggestions: Array<{ partner: number; give: string; get: string; partner_need: string; note: string }>; summary: string };

const round2 = (n: number) => Math.round(n * 100) / 100;
const ref = (p: TPlayer): TRef => ({ id: p.id, name: p.name, pos: p.pos, team: p.team, inj: p.inj, proj: p.proj, bye: p.bye });
export const partnerRef = (s: Side): PartnerRef => ({ rosterId: s.rosterId, team: s.team, record: s.record, stance: s.stance, needs: s.needs });
const total = (ps: TRef[]) => ps.reduce((n, p) => n + p.proj, 0);
const linked = (news: Record<string, NewsItem[]>, ps: TPlayer[]): Linked => ps.flatMap((p) => (news[p.id] ?? []).map((n) => ({ ...n, player: p.name })));

function pick(from: Side, ids: string[], who: string): TPlayer[] {
  const seen = new Set<string>();
  return ids.map((id) => {
    const p = from.players[id];
    if (!p) throw new Error(`the model used ${id}, who is not on ${who}`);
    if (seen.has(id)) throw new Error(`the model listed ${id} twice`);
    seen.add(id);
    return p;
  });
}

function offered(f: TradeFacts, partner: Side, give: string[], get: string[]): Offered & { players: TPlayer[] } {
  const g = pick(f.mine, give, 'your team');
  const r = pick(partner, get, partner.team);
  if (!g.length || !r.length) throw new Error('the model gave a trade with nothing on one side');
  const [give_, get_] = [g.map(ref), r.map(ref)];
  return { give: give_, get: get_, delta: { you: round2(total(get_) - total(give_)), partner: round2(total(give_) - total(get_)) }, players: [...g, ...r] };
}

/** The offer in the facts is the one that was asked about; the model supplies the verdict, a counter and the reasons. */
export function buildTradeCard(f: TradeFacts, d: TradeDecision, news: Record<string, NewsItem[]>): TradeCard {
  const partner = f.partners[0];
  if (!f.offer) throw new Error('a trade analysis needs an offer');
  const main = offered(f, partner, f.offer.give, f.offer.get);
  const counter = d.verdict === 'counter' ? d.counter : null;
  if (d.verdict === 'counter' && !counter) throw new Error('the model countered without a counter-offer');
  const alt = counter ? offered(f, partner, counter.give, counter.get) : null;
  const { players, ...first } = main;
  return {
    kind: 'trade', league: f.league, week: f.week, team: f.team, summary: d.summary, partner: partnerRef(partner),
    offers: [{ ...first, verdict: d.verdict, why: d.why, flips_if: d.flips_if, counter: alt && { give: alt.give, get: alt.get, delta: alt.delta }, news: linked(news, [...players, ...(alt?.players ?? [])]) }],
  };
}

/** Each suggestion trades one of your players for one of the partner's listed surplus players; ranked as the model ordered them. */
export function buildFinderCard(f: TradeFacts, d: FinderDecision, news: Record<string, NewsItem[]>): FinderCard {
  if (d.suggestions.length > MAX_SUGGESTIONS) throw new Error(`the model suggested ${d.suggestions.length} deals, at most ${MAX_SUGGESTIONS} are allowed`);
  const gives = d.suggestions.map((s) => s.give);
  if (new Set(gives).size < gives.length) throw new Error('the model offered the same player in two deals');
  const suggestions = d.suggestions.map((s, i) => {
    const partner = f.partners.find((p) => p.rosterId === s.partner);
    if (!partner) throw new Error(`the model picked team ${s.partner}, who is not a partner`);
    if (!partner.surplus.includes(s.get)) throw new Error(`the model asked for ${s.get}, who is not surplus on ${partner.team}`);
    const o = offered(f, partner, [s.give], [s.get]);
    return { rank: i + 1, partner: partnerRef(partner), partner_need: s.partner_need, give: o.give[0], get: o.get[0], delta: o.delta, note: s.note, news: linked(news, o.players) };
  });
  return { kind: 'trade-finder', league: f.league, week: f.week, team: f.team, summary: suggestions.length || /no deal/i.test(d.summary) ? d.summary : 'No deals: no partner has a surplus player this team needs.', suggestions };
}
