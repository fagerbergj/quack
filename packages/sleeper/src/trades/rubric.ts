import type { Declaration, Inputs, Rubric } from '@quack/judge';
import { parseCard, SHARE, yes } from '../rubric-kit.ts';
import type { FinderCard } from './card.ts';
import type { TradeFacts } from './facts.ts';

type Suggestion = FinderCard['suggestions'][number];

const brief = (s: Suggestion) => ({ id: `suggestion${s.rank}`, give: [s.give.name], get: [s.get.name], text: s.note });
const designations = (s: Suggestion) => Object.fromEntries([s.give, s.get].map((p) => [p.name, p.inj ?? 'none']));

/** Each suggestion as one record for the partner check. */
function suggestions(f: TradeFacts, card: FinderCard) {
  return card.suggestions.map((s) => {
    const p = f.partners.find((x) => x.rosterId === s.partner.rosterId);
    return {
      id: `suggestion${s.rank}`, partner_need: s.partner_need, give: s.give.name, get: s.get.name, note: s.note,
      partner: p ? { team: p.team, record: p.record, stance: p.stance, needs: p.needs, bye_holes: p.byes } : s.partner,
    };
  });
}

/** Each suggestion with news as one record for the news check. */
function newsRows(f: TradeFacts, card: FinderCard) {
  return card.suggestions.filter((s) => s.news.length > 0).map((s) => {
    const p = f.partners.find((x) => x.rosterId === s.partner.rosterId);
    return {
      id: `suggestion${s.rank}`, deal: brief(s), news: s.news, designations: designations(s), league_teams: f.teams,
      you: { stance: f.mine.stance, needs: f.mine.needs, surplus: f.mine.surplus },
      partner: p && { team: p.team, record: p.record, stance: p.stance, needs: p.needs, surplus: p.surplus },
    };
  });
}

/** The judge's named texts: the card's suggestions for partner and news checks. */
export function tradeInputsOf(f: TradeFacts, content: string): Inputs {
  const card = parseCard<FinderCard>(content);
  return { suggestions: JSON.stringify(suggestions(f, card)), news: JSON.stringify(newsRows(f, card)) };
}
export const tradeInputs = (f: TradeFacts) => async (_api: unknown, sealed: { content: string }): Promise<Inputs> => tradeInputsOf(f, sealed.content);

/** How each model check of tradeRubric is asked: per suggestion, each item alone with nothing shared. */
export const tradeDeclaration: Declaration = {
  scopes: {
    suggestions: { split: { name: 'jsonItems', from: 'suggestions', pick: ['partner', 'partner_need', 'give', 'get', 'note'] }, piece: 'suggestion' },
    // News is searched for every dealable player, so with no source no row has any and the check is not scored.
    news: { split: { name: 'jsonItems', from: 'news', pick: ['deal', 'news', 'designations', 'league_teams', 'you', 'partner'] }, piece: 'row' },
  },
  checks: {
    partner_modeled: { scope: 'suggestions', aggregate: SHARE, question: yes('Does `note` give a reason this partner would say yes from its `partner` row (its needs, bye holes, record or stance), not chart value alone?') },
    // Weighing the note against the reports is a claim against evidence, which the decision model passes unread, so the 27B decides it.
    news_per_traded_player: { scope: 'news', aggregate: SHARE, decisionModel: false, question: yes('Does each traded player have news cited, from the attached `news` and the Sleeper `designations`, naming the right player for each report?') },
  },
};

// The card builder takes every player, side and projection from the rosters, so no code gate is left to fail.
// Trade weights are unreplayed: replay them before any weight gates delivery.
export const tradeRubric: Rubric = {
  name: 'sleeper.trade',
  version: 7,
  threshold: 0.8,
  criteria: [
    { id: 'case_is_reasoned', checks: [
      { id: 'partner_modeled', label: "Ignores the other team's needs", weight: 4 },
      { id: 'news_per_traded_player', label: "A traded player has no news cited", weight: 3 },
    ] },
  ],
};
