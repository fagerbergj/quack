import type { JsonValue } from '@earendil-works/chord';
import { Type } from '@earendil-works/pi-ai';
import { rejudge, type JudgeOptions, type Verdict } from '@quack/judge';
import { attachVerdict, seal, type SealedArtifact } from '@quack/sdk';
import { defineWorkflow, type StepContext, type Step } from '@quack/workflows';
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

const verdictSchema = Type.Union(['send', 'counter', 'decline'].map((v) => Type.Literal(v)));
const tradeSchema = Type.Object({
  verdict: verdictSchema,
  why: Type.String({ maxLength: 350 }),
  flips_if: Type.String({ maxLength: 200 }),
  counter: Type.Optional(Type.Union([Type.Null(), Type.Object({ give: Type.Array(Type.String(), { minItems: 1 }), get: Type.Array(Type.String(), { minItems: 1 }) })])),
  summary: Type.String({ maxLength: 300 }),
});
const finderSchema = Type.Object({
  suggestions: Type.Array(Type.Object({ partner: Type.Integer(), give: Type.String(), get: Type.String(), partner_need: Type.String({ maxLength: 32 }), note: Type.String({ maxLength: 350 }) }), { maxItems: 3 }),
  summary: Type.String({ maxLength: 300 }),
});

const line = (p: TPlayer) => `${p.id} ${p.name} (${p.pos}, ${p.team ?? '-'}) proj ${p.proj}${p.inj ? `, ${p.inj}` : ''}${p.bye ? `, bye wk ${p.bye}` : ''}`;
const links = (news: News, id: string) => (news[id] ?? []).map((n) => ` [${n.title}](${n.url}), ${n.date.slice(0, 16)}`).join('');
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

/** facts, [news], decide, [news], card, check: the news step goes before the decision for an analysis and after it for the finder. */
function steps<I extends FinderInput>(deps: TradeJobDeps, o: { decide: Step; news: (f: TradeFacts, c: StepContext) => TPlayer[]; newsFirst: boolean; card: (f: TradeFacts, d: unknown, news: News) => unknown }): Step[] {
  const input = (c: StepContext) => JSON.parse(String(c.input)) as I;
  const newsStep: Step = {
    name: 'news',
    kind: 'code',
    run: async (c) => (await fetchNews(deps.news, o.news(prior(c, 'facts'), c), deps.now ? deps.now() : c.api.now())) as unknown as JsonValue,
  };
  const facts: Step = {
    name: 'facts',
    kind: 'code',
    run: async (c) => {
      const i = input(c);
      return (await buildTradeFacts({ client: deps.client }, await deps.snapshot(i.league), i)) as unknown as JsonValue;
    },
  };
  const card: Step = {
    name: 'card',
    kind: 'code',
    run: async (c) => {
      await c.api.env!.writeFile(cardsKind.path, JSON.stringify(o.card(prior(c, 'facts'), prior(c, 'decide'), prior(c, 'news')), null, 1), c.ctx);
      return (await seal(c.api, cardsKind, c.ctx)) as unknown as JsonValue;
    },
  };
  const check: Step = {
    name: 'check',
    kind: 'code',
    run: async (c) => {
      const sealed = prior<SealedArtifact>(c, 'card');
      const { options, notes } = judgeOf(deps, prior(c, 'facts'));
      const verdict: Verdict = await rejudge(c.api, options, sealed, `judge:${sealed.kind}:v${sealed.version}`, c.ctx);
      await attachVerdict(c.api, sealed.kind, sealed.version, { ...verdict, notes } as never, c.ctx);
      return { passed: verdict.passed, score: verdict.score, gatesFailed: verdict.gatesFailed, notes };
    },
  };
  return o.newsFirst ? [facts, newsStep, o.decide, card, check] : [facts, o.decide, newsStep, card, check];
}

const named = (f: TradeFacts, ids: string[]) => ids.flatMap((id) => playerOf(f, id) ?? []);

/** Trade analysis and trade talk (SL-1): facts for both rosters, news for each traded player, one verdict, a sealed card, then the judge. */
export function tradeWorkflow(deps: TradeJobDeps) {
  const decide: Step = { name: 'decide', kind: 'model', schema: tradeSchema, prompt: (c) => tradePrompt(prior(c, 'facts'), prior(c, 'news')) };
  return defineWorkflow({
    name: TRADE_WORKFLOW,
    steps: steps<TradeInput>(deps, { decide, newsFirst: true, news: (f) => named(f, [...f.offer!.give, ...f.offer!.get]), card: (f, d, n) => buildTradeCard(f, d as TradeDecision, n) }),
  });
}

/** Trade finder (SL-1): the partner table is code; the model picks at most three deals; code searches news for the players it picked. */
export function finderWorkflow(deps: TradeJobDeps) {
  const decide: Step = { name: 'decide', kind: 'model', schema: finderSchema, prompt: (c) => finderPrompt(prior(c, 'facts')) };
  return defineWorkflow({
    name: FINDER_WORKFLOW,
    steps: steps<FinderInput>(deps, { decide, newsFirst: false, news: (f, c) => named(f, (prior<FinderDecision>(c, 'decide').suggestions ?? []).flatMap((s) => [s.give, s.get])), card: (f, d, n) => buildFinderCard(f, d as FinderDecision, n) }),
  });
}
