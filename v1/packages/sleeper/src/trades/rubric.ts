import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import { MAX_SUGGESTIONS, VERDICTS, type FinderCard, type Offered, type TradeCard, type TRef } from './card.ts';
import type { TradeFacts } from './facts.ts';

type Card = TradeCard | FinderCard;
type Row = { id: string; partner: number; give: TRef[]; get: TRef[]; verdict?: string; text: string; news: TradeCard['offers'][number]['news']; counter?: Offered | null };

const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });
const parse = (artifact: string) => JSON.parse(artifact) as Card;
const factsOf = (ev: Evidence) => ev.facts[0].state as unknown as TradeFacts;

/** The card's trades as one shape: an offer is judged on its verdict and reasons, a suggestion on its note. */
function rowsOf(c: Card): Row[] {
  if (c.kind === 'trade-finder') return c.suggestions.map((s) => ({ id: `suggestion${s.rank}`, partner: s.partner.rosterId, give: [s.give], get: [s.get], text: s.note, news: s.news }));
  return c.offers.map((o, i) => ({ id: `offer${i + 1}`, partner: c.partner.rosterId, give: o.give, get: o.get, verdict: o.verdict, text: `${o.why} Flips if: ${o.flips_if}`, news: o.news, counter: o.counter }));
}

function verdictOneOf(artifact: string): CodeResult {
  const c = parse(artifact);
  if (c.kind === 'trade-finder') {
    const ranked = c.suggestions.every((s, i) => s.rank === i + 1);
    return { met: c.suggestions.length <= MAX_SUGGESTIONS && ranked ? 1 : 0, detail: `${c.suggestions.length} suggestions, ranked ${ranked}; at most ${MAX_SUGGESTIONS}, ranked from 1` };
  }
  const bad = c.offers.filter((o) => !VERDICTS.includes(o.verdict) || (o.verdict === 'counter') !== !!o.counter);
  return share(c.offers.length - bad.length, c.offers.length, `verdict must be ${VERDICTS.join(', ')}, with a counter exactly when it is counter`);
}

const key = (ps: TRef[]) => ps.map((p) => p.id).sort().join(',');

// Ids are checked against the rosters in the tools data, and an analysis against the offer that was asked about.
function giveGetFromMySide(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const rows = rowsOf(parse(artifact));
  const sides = (r: Row) => [{ ps: r.give, from: f.mine.players }, { ps: r.get, from: f.partners.find((p) => p.rosterId === r.partner)?.players ?? {} }];
  const bad = rows.filter((r) => sides(r).some(({ ps, from }) => !ps.length || ps.some((p) => from[p.id]?.proj !== p.proj)));
  const main = rows[0];
  const asked = !f.offer || !main || (key(main.give) === [...f.offer.give].sort().join(',') && key(main.get) === [...f.offer.get].sort().join(','));
  const problems = [...(bad.length ? [`${bad.map((r) => r.id).join(', ')} name players or projections that differ from the rosters`] : []), ...(asked ? [] : ['the first offer is not the trade that was asked about'])];
  return share(rows.length - bad.length - (asked ? 0 : 1), rows.length, problems.join('; '));
}

const names = (r: Row) => Object.fromEntries([...r.give, ...r.get].map((p) => [p.name, p.inj ?? 'none']));
const brief = (r: Row) => ({ id: r.id, verdict: r.verdict, give: r.give.map((p) => p.name), get: r.get.map((p) => p.name), text: r.text });
const multi = (ps: { give: TRef[]; get: TRef[] }) => ps.give.length > 1 || ps.get.length > 1;

/** Row units carry the trade, both sides' designations, the partner's table row and the news attached by code. */
export function tradeEvidence(f: TradeFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse(sealed.content);
    const rows = rowsOf(card);
    const unit = (r: Row) => {
      const partner = f.partners.find((p) => p.rosterId === r.partner);
      return { id: r.id, state: { row: brief(r), counter: r.counter ?? null, news: r.news, designations: names(r), league_teams: f.teams, you: { stance: f.mine.stance, needs: f.mine.needs, surplus: f.mine.surplus }, partner: partner && { team: partner.team, record: partner.record, stance: partner.stance, needs: partner.needs, surplus: partner.surplus } } as never };
    };
    return {
      facts: [{ id: 'facts', state: f as never }],
      row: rows.map(unit),
      consolidation: rows.filter((r) => multi(r) || (r.counter && multi(r.counter))).map(unit),
      card: [{ id: 'card', state: { summary: card.summary, league_teams: f.teams, rows: rows.map(brief) } as never }],
    };
  };
}

// The audit's trade table (docs/v1/audits/sleeper.md), unreplayed: D45 asks for replay before any weight gates delivery.
export const tradeRubric: Rubric = {
  name: 'sleeper.trade',
  version: 1,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [
      { id: 'verdict_one_of', weight: 'gate', code: verdictOneOf },
      { id: 'give_get_from_my_side', weight: 'gate', code: giveGetFromMySide },
    ] },
    { id: 'case_is_reasoned', checks: [
      { id: 'partner_modeled', weight: 4, scope: 'card', question: 'Does the case rest on the partner\'s starting slots, record and surplus from the `partner` table, not on chart value alone?' },
      { id: 'deciding_line_item', weight: 3, scope: 'card', question: 'Does it name the one line item that decides the call, with a condition that would flip it?' },
      { id: 'news_per_traded_player', weight: 3, scope: 'row', question: 'Does each traded player have news cited, from the attached `news` and the Sleeper `designations`, naming the right player for each report?' },
      { id: 'consolidation_priced', weight: 2, scope: 'consolidation', question: 'This is a multi-for-one. Does it price the consolidation discount and the cut it forces on the side receiving more players?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'chart_without_mismatch', weight: -3, scope: 'card', question: 'Does it quote a 12-team value chart without saying the league has fewer teams (`league_teams`)?' },
      { id: 'one_sided_as_collusion', weight: -3, scope: 'card', question: 'Does it call a lopsided but voluntary deal collusion?' },
    ] },
  ],
};
