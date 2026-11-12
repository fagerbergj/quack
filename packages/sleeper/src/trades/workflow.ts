import type { JsonValue } from '@earendil-works/chord';
import { arr, int, obj, str } from '@earendil-works/pi-schema';
import { defineWorkflow, type StepContext, type Step } from '@quack/workflows';
import { newsFor, newsLink } from '../lineup/news.ts';
import { liveLeague } from '../snapshot.ts';
import type { NewsItem } from '../lineup/facts.ts';
import type { LineupDeps } from '../lineup/workflow.ts';
import { buildFinderCard, type FinderDecision } from './card.ts';
import { buildTradeFacts, dealable, type FinderInput, type Side, type TPlayer, type TradeFacts } from './facts.ts';
import { tradeDeclaration, tradeInputs, tradeRubric } from './rubric.ts';
import { cardJudge } from '../job.ts';

export const FINDER_WORKFLOW = 'sleeper.trade-finder';

/** The same wiring as the other jobs: the Sleeper API and an optional news source. */
export type TradeJobDeps = LineupDeps;
type News = Record<string, NewsItem[]>;

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

const prior = <T>(c: StepContext, step: string) => c.prior[step] as unknown as T;

const at = (deps: TradeJobDeps, c: StepContext) => (deps.now ? deps.now() : c.api.now());

/** Trade finder: the partner table is code; news for every dealable player is searched once, before the model picks at most three deals. */
export function finderWorkflow(deps: TradeJobDeps) {
  const facts: Step = {
    name: 'facts',
    kind: 'code',
    run: async (c) => {
      const i = JSON.parse(String(c.input)) as FinderInput;
      return (await buildTradeFacts({ client: deps.client }, await liveLeague(deps.client, i.league), i)) as unknown as JsonValue;
    },
  };
  const news: Step = { name: 'news', kind: 'code', run: async (c) => (await newsFor(deps.news, dealable(prior(c, 'facts')), at(deps, c))) as unknown as JsonValue };
  const judgeOf = cardJudge(deps, tradeRubric, undefined, { declaration: tradeDeclaration, inputs: tradeInputs });
  const card: Step = {
    name: 'card',
    kind: 'judged',
    schema: finderSchema,
    prompt: (c) => finderPrompt(prior(c, 'facts'), prior(c, 'news')),
    card: (c, d) => buildFinderCard(prior(c, 'facts'), d as unknown as FinderDecision, prior(c, 'news')),
    judge: (c) => judgeOf(prior(c, 'facts')),
  };
  return defineWorkflow({ name: FINDER_WORKFLOW, steps: [facts, news, card] });
}
