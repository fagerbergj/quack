import type { Evidence, Rubric } from '@quack/judge';
import { parseCard } from '../rubric-kit.ts';
import type { FinderCard, Offered, TradeCard, TRef } from './card.ts';
import type { TradeFacts } from './facts.ts';

type Card = TradeCard | FinderCard;
type Row = { id: string; partner: number; give: TRef[]; get: TRef[]; verdict?: string; text: string; news: TradeCard['offers'][number]['news']; counter?: Offered | null };

const parse = (artifact: string) => parseCard<Card>(artifact);

/** The card's trades as one shape: an offer is judged on its verdict and reasons, a suggestion on its note. */
function rowsOf(c: Card): Row[] {
  if (c.kind === 'trade-finder') return c.suggestions.map((s) => ({ id: `suggestion${s.rank}`, partner: s.partner.rosterId, give: [s.give], get: [s.get], text: s.note, news: s.news }));
  return c.offers.map((o, i) => ({ id: `offer${i + 1}`, partner: c.partner.rosterId, give: o.give, get: o.get, verdict: o.verdict, text: `${o.why} Flips if: ${o.flips_if}`, news: o.news, counter: o.counter }));
}

const names = (r: Row) => Object.fromEntries([...r.give, ...r.get].map((p) => [p.name, p.inj ?? 'none']));
const brief = (r: Row) => ({ id: r.id, verdict: r.verdict, give: r.give.map((p) => p.name), get: r.get.map((p) => p.name), text: r.text });
const multi = (ps: { give: TRef[]; get: TRef[] }) => ps.give.length > 1 || ps.get.length > 1;

/** Row units carry the trade, both sides' designations, the partner's table row and the news attached by code. */
export function tradeEvidence(f: TradeFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse(sealed.content);
    const rows = rowsOf(card);
    const cardUnit = { id: 'card', whole: true as const, state: { summary: card.summary, league_teams: f.teams, rows: rows.map(brief) } as never };
    const unit = (r: Row) => {
      const partner = f.partners.find((p) => p.rosterId === r.partner);
      return { id: r.id, state: { row: brief(r), counter: r.counter ?? null, news: r.news, designations: names(r), league_teams: f.teams, you: { stance: f.mine.stance, needs: f.mine.needs, surplus: f.mine.surplus }, partner: partner && { team: partner.team, record: partner.record, stance: partner.stance, needs: partner.needs, surplus: partner.surplus } } as never };
    };
    return {
      facts: [{ id: 'facts', whole: true as const, state: f as never }],
      row: rows.map(unit),
      consolidation: rows.filter((r) => multi(r) || (r.counter && multi(r.counter))).map(unit),
      card: [cardUnit],
      // With no suggestions there is no call to decide, so the check has no unit.
      suggested: rows.length ? [cardUnit] : [],
    };
  };
}

// The schema fixes the verdict and the card builder takes every player, side and projection from the rosters, so no code gate is left to fail.
// Trade weights are unreplayed: replay them before any weight gates delivery.
export const tradeRubric: Rubric = {
  name: 'sleeper.trade',
  version: 4,
  threshold: 0.8,
  criteria: [
    { id: 'case_is_reasoned', checks: [
      { id: 'partner_modeled', label: "Ignores the other team's needs", weight: 4, scope: 'card', question: 'Does the case rest on the partner\'s starting slots, record and surplus from the `partner` table, not on chart value alone?' },
      { id: 'deciding_line_item', label: "Doesn't name what decides the trade", weight: 3, scope: 'suggested', question: 'Does it name the one line item that decides the call, with a condition that would flip it?' },
      { id: 'news_per_traded_player', label: "A traded player has no news cited", weight: 3, scope: 'row', question: 'Does each traded player have news cited, from the attached `news` and the Sleeper `designations`, naming the right player for each report?' },
      { id: 'consolidation_priced', label: "Doesn't price giving up depth", weight: 2, scope: 'consolidation', question: 'This is a multi-for-one. Does it price the consolidation discount and the cut it forces on the side receiving more players?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'chart_without_mismatch', label: "Uses a 12-team chart in a smaller league", weight: -3, scope: 'card', question: 'Does it quote a 12-team value chart without saying the league has fewer teams (`league_teams`)?' },
      { id: 'one_sided_as_collusion', label: "Calls a lopsided deal collusion", weight: -3, scope: 'card', question: 'Does it call a lopsided but voluntary deal collusion?' },
    ] },
  ],
};
