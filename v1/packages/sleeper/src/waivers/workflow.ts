import type { JsonValue } from '@earendil-works/chord';
import { arr, int, obj, opt, str } from '@earendil-works/pi-schema';
import { defineDoc, type JsonObject } from '@earendil-works/pi-durable';
import { type JudgeOptions } from '@quack/judge';
import { defineWorkflow, type StepContext } from '@quack/workflows';
import { newsLink } from '../lineup/news.ts';
import { cardsKind } from '../lineup/card.ts';
import type { LineupDeps } from '../lineup/workflow.ts';
import type { LineupInput } from '../lineup/facts.ts';
import { buildWaiversCard, type WaiverDecision } from './card.ts';
import { buildWaiverFacts, type Valued, type WaiverFacts } from './facts.ts';
import { waiversEvidence, waiversRubric } from './rubric.ts';

export const WAIVERS_WORKFLOW = 'sleeper.waivers';

/** The facts the card was judged on, kept in the session so eval can rebuild the judge's evidence from a recording. */
export const WAIVERS_FACTS_KIND = 'sleeper.waivers.facts';
export const WaiversFactsDoc = defineDoc<{ facts: JsonObject | null }>({ kind: WAIVERS_FACTS_KIND, version: 1, scope: 'conversation', history: 'latest', fork: 'initial', initial: () => ({ facts: null }), checkpointWhen: () => false });

/** The same wiring as the lineup job: the Sleeper API, today's snapshot and an optional news source. */
export type WaiversDeps = LineupDeps;

const decisionSchema = obj({
  claims: arr(obj({ add: str(), drop: str(), bid: opt(int({ minimum: 0 })), why: str({ maxLength: 240 }) }, { open: true }), { maxItems: 6 }),
  passed: arr(obj({ playerId: str(), why: str({ maxLength: 160 }) }, { open: true })),
  summary: str({ maxLength: 400 }),
}, { open: true });

const line = (p: Valued) => `${p.id} ${p.name} (${p.pos}, ${p.team ?? '-'}) proj ${p.proj}${p.ppg === undefined ? '' : `, season ${p.ppg}/g`}${p.inj ? `, ${p.inj}` : ''}${p.bye ? `, bye wk ${p.bye}` : ''}`;
const links = (f: WaiverFacts, id: string) => (f.news[id] ?? []).map((n) => ` ${newsLink(n)}`).join('');

function factsPrompt(f: WaiverFacts): string {
  const faab = f.waiverType === 'faab';
  const mine = f.table.find((t) => t.mine)!;
  const teams = f.table.map((t) => `${t.mine ? 'YOU ' : ''}${t.team}: ${faab ? `$${t.faabLeft} left` : `priority ${t.priority}`}; needs ${JSON.stringify(t.needs)}; bye holes ${JSON.stringify(t.byes)}`);
  const wide = new Set(f.widely);
  const free = Object.entries(f.buckets).map(([pos, ids]) => `${pos}\n${ids.map((id) => `  - ${line(f.free[id])}, owned ${f.free[id].owned ?? '?'}%, ${f.free[id].adds ?? 'no'} adds/24h${wide.has(id) ? ' [MARKED]' : ''}${links(f, id)}`).join('\n')}`);
  const roster = Object.values(f.roster).sort((a, b) => a.role.localeCompare(b.role)).map((p) => `  - ${line(p)} [${p.role}]${links(f, p.id)}`);
  return [
    `Week ${f.week}, ${f.team}. Waiver type: ${f.waiverType}${faab ? `, $${mine.faabLeft} left` : `, your priority ${f.myPriority} of ${f.teams}`}.`,
    `League competition:\n${teams.join('\n')}`,
    `Free agents by position (ownership and adds are platform-wide):\n${free.join('\n')}`,
    `Your roster:\n${roster.join('\n')}`,
    ...(Object.values(f.news).some((n) => n.length) ? ['Linked news is dated. A newer report from a credible source beats the Sleeper designation, which lags; when they conflict, say which you followed.'] : []),
    `Rank your claims by id. Every [MARKED] player is a claim or goes in passed with a reason.${faab ? ' Give each bid as whole dollars.' : ' Give no bid: this league has none.'}`,
  ].join('\n\n');
}

/** Waivers job (SL-1): facts, one model decision, a sealed card, then the judge's verdict on it. */
export function waiversWorkflow(deps: WaiversDeps) {
  const input = (c: StepContext) => JSON.parse(String(c.input)) as LineupInput;
  // close_call_news cannot be met without a news source, so it is scored only once one is wired; the verdict says so.
  const rubric = deps.news ? waiversRubric : { ...waiversRubric, criteria: waiversRubric.criteria.map((c) => ({ ...c, checks: c.checks.filter((k) => k.id !== 'close_call_news') })) };
  const notes = deps.news ? [] : ['close-call news not checked: no news source'];
  const judgeOptions = (f: WaiverFacts): JudgeOptions => ({ kind: cardsKind, rubric, evidence: waiversEvidence(f) as never, maxRevisions: 1, classifier: deps.classifier });
  return defineWorkflow({
    name: WAIVERS_WORKFLOW,
    steps: [
      {
        name: 'facts',
        kind: 'code',
        run: async (c) => {
          const i = input(c);
          const facts = (await buildWaiverFacts({ client: deps.client, news: deps.news }, await deps.snapshot(i.league), i, deps.now ? deps.now() : c.api.now())) as unknown as JsonObject;
          await c.api.commit(async (tx) => { (await tx.doc(WaiversFactsDoc, c.api.conversationId)).facts = facts; }, c.ctx);
          return facts;
        },
      },
      {
        name: 'card',
        kind: 'judged',
        schema: decisionSchema,
        prompt: (c) => factsPrompt(c.prior.facts as unknown as WaiverFacts),
        card: (c, d) => buildWaiversCard(c.prior.facts as unknown as WaiverFacts, d as unknown as WaiverDecision),
        judge: (c) => ({ options: judgeOptions(c.prior.facts as unknown as WaiverFacts), notes }),
      },
    ],
  });
}
