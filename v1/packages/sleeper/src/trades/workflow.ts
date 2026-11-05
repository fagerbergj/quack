import type { JsonValue } from '@earendil-works/chord';
import { arr, int, lit, nul, obj, opt, str, union } from '@earendil-works/pi-schema';
import type { JudgeOptions } from '@quack/judge';
import { defineWorkflow, type StepContext, type Step } from '@quack/workflows';
import { newsLink } from '../lineup/news.ts';
import { cardsKind } from '../lineup/card.ts';
import type { NewsItem } from '../lineup/facts.ts';
import type { LineupDeps } from '../lineup/workflow.ts';
import { buildFinderCard, buildTradeCard, type FinderDecision, type TradeDecision } from './card.ts';
import { buildTradeFacts, fetchNews, playerOf, type FinderInput, type Side, type TPlayer, type TradeFacts, type TradeInput } from './facts.ts';
import { tradeEvidence, tradeRubric } from './rubric.ts';

export const TRADE_WORKFLOW = 'sleeper.trade';
export const FINDER_WORKFLOW = 'sleeper.trade-finder';

/** The same wiring as the other jobs: the Sleeper API, today's snapshot and an optional news source. */
export type TradeJobDeps = LineupDeps;
type News = Record<string, NewsItem[]>;

const verdictSchema = union(['send', 'counter', 'decline'].map((v) => lit(v)));
const tradeSchema = obj({
  verdict: verdictSchema,
  why: str({ maxLength: 350 }),
  flips_if: str({ maxLength: 200 }),
  counter: opt(union([nul(), obj({ give: arr(str(), { minItems: 1 }), get: arr(str(), { minItems: 1 }) }, { open: true })])),
  summary: str({ maxLength: 300 }),
}, { open: true });
const finderSchema = obj({
  suggestions: arr(obj({ partner: int(), give: str(), get: str(), partner_need: str({ maxLength: 32 }), note: str({ maxLength: 350 }) }, { open: true }), { maxItems: 3 }),
  summary: str({ maxLength: 300 }),
}, { open: true });

const line = (p: TPlayer) => `${p.id} ${p.name} (${p.pos}, ${p.team ?? '-'}) proj ${p.proj}${p.inj ? `, ${p.inj}` : ''}${p.bye ? `, bye wk ${p.bye}` : ''}`;
const links = (news: News, id: string) => (news[id] ?? []).map((n) => ` ${newsLink(n)}`).join('');
const roster = (s: Side, news: News) => Object.values(s.players).sort((a, b) => a.role.localeCompare(b.role) || b.proj - a.proj).map((p) => `  - ${line(p)} [${p.role}]${links(news, p.id)}`).join('\n');
const standing = (s: Side, of: number) => `${s.team} (${s.record}, ${s.rank} of ${of}, ${s.stance}); needs ${JSON.stringify(s.needs)}; bye holes ${JSON.stringify(s.byes)}`;
const header = (f: TradeFacts) => `Week ${f.week}, ${standing(f.mine, f.teams)}. The league has ${f.teams} teams and ${f.playoffTeams} playoff spots; public trade charts assume 12 teams.`;

function finderPrompt(f: TradeFacts): string {
  const partners = f.partners.map((p) => `[${p.rosterId}] ${standing(p, f.teams)}\n  Surplus (the only players you may ask for):\n${p.surplus.map((id) => `    - ${line(p.players[id])}`).join('\n') || '    none'}`);
  return [
    header(f),
    `Your roster; your surplus ids: ${JSON.stringify(f.mine.surplus)}:\n${roster(f.mine, {})}`,
    `Partners:\n${partners.join('\n')}`,
    'Choose at most three deals, best first. Each trades one of your players (give) for one of that team\'s surplus players (get); name the team by its bracketed roster id. With no fit, return none and say so.',
  ].join('\n\n');
}

function tradePrompt(f: TradeFacts, news: News): string {
  const p = f.partners[0];
  const ids = (xs: string[], s: Side) => xs.map((id) => `  - ${line(s.players[id])}${links(news, id)}`).join('\n');
  return [
    header(f),
    `Partner: [${p.rosterId}] ${standing(p, f.teams)}; surplus ${JSON.stringify(p.surplus)}`,
    `The offer, from your side. You give:\n${ids(f.offer!.give, f.mine)}\nYou get:\n${ids(f.offer!.get, p)}`,
    `Your roster (you need ${JSON.stringify(f.mine.needs)}):\n${roster(f.mine, news)}`,
    `${p.team}'s roster:\n${roster(p, news)}`,
    'Linked news is dated. A newer report from a credible source beats the Sleeper designation, which lags; when they conflict, say which you followed.',
    'Give the verdict. For a counter, list the player ids on each side of your counter-offer, still from your side.',
  ].join('\n\n');
}

const judgeOf = (deps: TradeJobDeps, f: TradeFacts) => {
  // news_per_traded_player cannot be met without a news source, so it is scored only once one is wired; the verdict says so.
  const rubric = deps.news ? tradeRubric : { ...tradeRubric, criteria: tradeRubric.criteria.map((c) => ({ ...c, checks: c.checks.filter((k) => k.id !== 'news_per_traded_player') })) };
  const options: JudgeOptions = { kind: cardsKind, rubric, evidence: tradeEvidence(f) as never, maxRevisions: 1, classifier: deps.classifier };
  return { options, notes: deps.news ? [] : ['news not checked: no news source'] };
};

const prior = <T>(c: StepContext, step: string) => c.prior[step] as unknown as T;

/** facts, [news], then one judged step: the news step goes before the decision for an analysis, and the finder fetches it for the players it picked while building the card. */
function steps<I extends FinderInput>(deps: TradeJobDeps, o: { decide: Pick<Extract<Step, { kind: 'judged' }>, 'schema' | 'prompt'>; news?: (f: TradeFacts) => TPlayer[]; card: (c: StepContext, d: unknown) => object | Promise<object> }): Step[] {
  const input = (c: StepContext) => JSON.parse(String(c.input)) as I;
  const facts: Step = {
    name: 'facts',
    kind: 'code',
    run: async (c) => {
      const i = input(c);
      return (await buildTradeFacts({ client: deps.client }, await deps.snapshot(i.league), i)) as unknown as JsonValue;
    },
  };
  const news: Step[] = o.news ? [{ name: 'news', kind: 'code', run: async (c) => (await fetchNews(deps.news, o.news!(prior(c, 'facts')), at(deps, c))) as unknown as JsonValue }] : [];
  const card: Step = { name: 'card', kind: 'judged', ...o.decide, card: o.card, judge: (c) => judgeOf(deps, prior(c, 'facts')) };
  return [facts, ...news, card];
}

const at = (deps: TradeJobDeps, c: StepContext) => (deps.now ? deps.now() : c.api.now());

const named = (f: TradeFacts, ids: string[]) => ids.flatMap((id) => playerOf(f, id) ?? []);

/** Trade analysis and trade talk (SL-1): facts for both rosters, news for each traded player, one verdict, a sealed card, then the judge. */
export function tradeWorkflow(deps: TradeJobDeps) {
  const decide = { schema: tradeSchema, prompt: (c: StepContext) => tradePrompt(prior(c, 'facts'), prior(c, 'news')) };
  return defineWorkflow({
    name: TRADE_WORKFLOW,
    steps: steps<TradeInput>(deps, { decide, news: (f) => named(f, [...f.offer!.give, ...f.offer!.get]), card: (c, d) => buildTradeCard(prior(c, 'facts'), d as TradeDecision, prior(c, 'news')) }),
  });
}

/** Trade finder (SL-1): the partner table is code; the model picks at most three deals; code searches news for the players it picked. */
export function finderWorkflow(deps: TradeJobDeps) {
  const decide = { schema: finderSchema, prompt: (c: StepContext) => finderPrompt(prior(c, 'facts')) };
  const card = async (c: StepContext, d: unknown) => {
    const f = prior<TradeFacts>(c, 'facts');
    const picks = named(f, ((d as FinderDecision).suggestions ?? []).flatMap((x) => [x.give, x.get]));
    return buildFinderCard(f, d as FinderDecision, await fetchNews(deps.news, picks, at(deps, c)));
  };
  return defineWorkflow({ name: FINDER_WORKFLOW, steps: steps<FinderInput>(deps, { decide, card }) });
}
