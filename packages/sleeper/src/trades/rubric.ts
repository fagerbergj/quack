import { registerSplitter, type Declaration, type Inputs, type Rubric } from '@quack/judge';
import { defect, metaFields, parseCard, SHARE, yes } from '../rubric-kit.ts';
import type { FinderCard } from './card.ts';
import type { TradeFacts } from './facts.ts';

type Suggestion = FinderCard['suggestions'][number];

const brief = (s: Suggestion) => ({ id: `suggestion${s.rank}`, give: [s.give.name], get: [s.get.name], text: s.note });
const designations = (s: Suggestion) => Object.fromEntries([s.give, s.get].map((p) => [p.name, p.inj ?? 'none']));

/** Each suggestion as one record: what the partner check reads, and the legacy unit news_per_traded_player keeps. */
function suggestions(f: TradeFacts, card: FinderCard) {
  return card.suggestions.map((s) => {
    const p = f.partners.find((x) => x.rosterId === s.partner.rosterId);
    return {
      id: `suggestion${s.rank}`, rank: s.rank, partner_need: s.partner_need, give: s.give.name, get: s.get.name, note: s.note,
      partner: p ? { team: p.team, record: p.record, stance: p.stance, needs: p.needs, bye_holes: p.byes } : s.partner,
      legacy: { row: brief(s), news: s.news, designations: designations(s), league_teams: f.teams, you: { stance: f.mine.stance, needs: f.mine.needs, surplus: f.mine.surplus }, partner: p && { team: p.team, record: p.record, stance: p.stance, needs: p.needs, surplus: p.surplus } },
    };
  });
}

/** The judge's named texts: the card's suggestions, and the card as the chart check reads it. */
export function tradeInputsOf(f: TradeFacts, content: string): Inputs {
  const card = parseCard<FinderCard>(content);
  return { suggestions: JSON.stringify(suggestions(f, card)), card: JSON.stringify({ summary: card.summary, league_teams: f.teams, rows: card.suggestions.map(brief) }) };
}
export const tradeInputs = (f: TradeFacts) => async (_api: unknown, sealed: { content: string }): Promise<Inputs> => tradeInputsOf(f, sealed.content);

// The news check stays on the 27B with its legacy unit: the card's row, then the other fields at the top.
type Row = ReturnType<typeof suggestions>[number];
registerSplitter('sleeper.tradeRows', (inputs, o) => (JSON.parse(inputs[String(o.from)]) as Row[]).map((r) => ({ id: r.id, text: JSON.stringify(r.legacy.row), meta: r.legacy })));

/** How each model check of tradeRubric is asked: per suggestion or of the card, each item alone with nothing shared. */
export const tradeDeclaration: Declaration = {
  scopes: {
    suggestions: { split: { name: 'jsonItems', from: 'suggestions', pick: ['partner', 'partner_need', 'give', 'get', 'note'] }, piece: 'suggestion' },
    // News is searched for every dealable player, so with no source no row has any and the check is not scored.
    news: { split: { name: 'sleeper.tradeRows', from: 'suggestions', where: [{ path: 'news', op: 'truthy' }] }, piece: 'row', context: metaFields(['news', 'designations', 'league_teams', 'you', 'partner']) },
    card: { split: { name: 'whole', from: 'card' }, piece: 'card' },
  },
  checks: {
    partner_modeled: { scope: 'suggestions', aggregate: SHARE, question: yes('Does `note` give a reason this partner would say yes from its `partner` row (its needs, bye holes, record or stance), not chart value alone?') },
    // Weighing the note against the reports is a claim against evidence, which the decision model passes unread, so the 27B decides it.
    news_per_traded_player: { scope: 'news', aggregate: SHARE, decisionModel: false, question: yes('Does each traded player have news cited, from the attached `news` and the Sleeper `designations`, naming the right player for each report?') },
    chart_without_mismatch: { scope: 'card', aggregate: SHARE, question: defect('Does it quote a 12-team value chart without saying the league has fewer teams (`league_teams`)?') },
  },
};

// The card builder takes every player, side and projection from the rosters, so no code gate is left to fail.
// Trade weights are unreplayed: replay them before any weight gates delivery.
export const tradeRubric: Rubric = {
  name: 'sleeper.trade',
  version: 6,
  threshold: 0.8,
  criteria: [
    { id: 'case_is_reasoned', checks: [
      { id: 'partner_modeled', label: "Ignores the other team's needs", weight: 4 },
      { id: 'news_per_traded_player', label: "A traded player has no news cited", weight: 3 },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'chart_without_mismatch', label: "Uses a 12-team chart in a smaller league", weight: -3 },
    ] },
  ],
};
