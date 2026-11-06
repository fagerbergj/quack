import type { JsonValue } from '@earendil-works/chord';
import { arr, int, lit, nul, obj, opt, str, union } from '@earendil-works/pi-schema';
import type { JudgeOptions } from '@quack/judge';
import { defineWorkflow, type StepContext, type Step } from '@quack/workflows';
import { newsFor, newsLink } from '../lineup/news.ts';
import { cardsKind } from '../lineup/card.ts';
import { liveLeague } from '../snapshot.ts';
import type { NewsItem } from '../lineup/facts.ts';
import type { LineupDeps } from '../lineup/workflow.ts';
import { buildFinderCard, buildTradeCard, type FinderDecision, type TradeDecision } from './card.ts';
import { buildTradeFacts, dealable, playerOf, type FinderInput, type Side, type TPlayer, type TradeFacts, type TradeInput } from './facts.ts';
import { tradeEvidence, tradeRubric } from './rubric.ts';
import { newsGated } from '../rubric-kit.ts';

export const TRADE_WORKFLOW = 'sleeper.trade';
export const FINDER_WORKFLOW = 'sleeper.trade-finder';

/** The same wiring as the other jobs: the Sleeper API and an optional news source. */
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

const NEWS_RULE = 'Linked news is dated. A newer report from a credible source beats the Sleeper designation, which lags; when they conflict, say which you followed.';

function finderPrompt(f: TradeFacts, news: News): string {
  const partners = f.partners.map((p) => `[${p.rosterId}] ${standing(p, f.teams)}\n  Surplus (the only players you may ask for):\n${p.surplus.map((id) => `    - ${line(p.players[id])}${links(news, id)}`).join('\n') || '    none'}`);
  return [
    header(f),
    `Your roster; your surplus ids: ${JSON.stringify(f.mine.surplus)}:\n${roster(f.mine, news)}`,
    `Partners:\n${partners.join('\n')}`,
    ...(Object.values(news).some((n) => n.length) ? [NEWS_RULE] : []),
    'Choose at most three deals, best first. Each trades one of your players (give) for one of that team\'s surplus players (get); name the team by its bracketed roster id. In `note`, cite the linked news for both players of the deal, or say none was found. With no fit, return none and say so.',
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
    NEWS_RULE,
    'Give the verdict. For a counter, list the player ids on each side of your counter-offer, still from your side.',
  ].join('\n\n');
}

const judgeOf = (deps: TradeJobDeps, f: TradeFacts) => {
  const { rubric, notes } = newsGated(tradeRubric, ['news_per_traded_player'], !!deps.news, 'news');
  const options: JudgeOptions = { kind: cardsKind, rubric, evidence: tradeEvidence(f) as never, maxRevisions: 1, classifier: deps.classifier };
  return { options, notes };
};

const prior = <T>(c: StepContext, step: string) => c.prior[step] as unknown as T;

/** facts, news, then one judged step: the news is searched once, before the decision, for every player the decision may name. */
function steps<I extends FinderInput>(deps: TradeJobDeps, o: { decide: Pick<Extract<Step, { kind: 'judged' }>, 'schema' | 'prompt'>; news: (f: TradeFacts) => TPlayer[]; card: (c: StepContext, d: unknown) => object | Promise<object> }): Step[] {
  const input = (c: StepContext) => JSON.parse(String(c.input)) as I;
  const facts: Step = {
    name: 'facts',
    kind: 'code',
    run: async (c) => {
      const i = input(c);
      return (await buildTradeFacts({ client: deps.client }, await liveLeague(deps.client, i.league), i)) as unknown as JsonValue;
    },
  };
  const news: Step = { name: 'news', kind: 'code', run: async (c) => (await newsFor(deps.news, o.news(prior(c, 'facts')), at(deps, c))) as unknown as JsonValue };
  const card: Step = { name: 'card', kind: 'judged', ...o.decide, card: o.card, judge: (c) => judgeOf(deps, prior(c, 'facts')) };
  return [facts, news, card];
}

const at = (deps: TradeJobDeps, c: StepContext) => (deps.now ? deps.now() : c.api.now());

const named = (f: TradeFacts, ids: string[]) => ids.flatMap((id) => playerOf(f, id) ?? []);

/** Trade analysis and trade talk: facts for both rosters, news for each traded player, one verdict, a sealed card, then the judge. */
export function tradeWorkflow(deps: TradeJobDeps) {
  const decide = { schema: tradeSchema, prompt: (c: StepContext) => tradePrompt(prior(c, 'facts'), prior(c, 'news')) };
  return defineWorkflow({
    name: TRADE_WORKFLOW,
    steps: steps<TradeInput>(deps, { decide, news: (f) => named(f, [...f.offer!.give, ...f.offer!.get]), card: (c, d) => buildTradeCard(prior(c, 'facts'), d as TradeDecision, prior(c, 'news')) }),
  });
}

/** Trade finder: the partner table is code; news for every dealable player is searched before the model picks at most three deals. */
export function finderWorkflow(deps: TradeJobDeps) {
  const decide = { schema: finderSchema, prompt: (c: StepContext) => finderPrompt(prior(c, 'facts'), prior(c, 'news')) };
  const card = (c: StepContext, d: unknown) => buildFinderCard(prior(c, 'facts'), d as FinderDecision, prior(c, 'news'));
  return defineWorkflow({ name: FINDER_WORKFLOW, steps: steps<FinderInput>(deps, { decide, news: dealable, card }) });
}
