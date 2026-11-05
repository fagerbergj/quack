import type { JsonValue } from '@earendil-works/chord';
import { arr, obj, str } from '@earendil-works/pi-schema';
import { type ClassifierRef, type JudgeOptions } from '@quack/judge';
import { defineWorkflow, type StepContext } from '@quack/workflows';
import { newsLink } from './news.ts';
import type { Json, SleeperClient } from '../client.ts';
import { buildCard, cardsKind, type Decision } from './card.ts';
import { buildFacts, type Facts, type FactsDeps, type LineupInput, type PlayerFact } from './facts.ts';
import { lineupEvidence, lineupRubric } from './rubric.ts';

export const LINEUP_WORKFLOW = 'sleeper.lineup';

export type LineupDeps = {
  client: SleeperClient;
  /** Today's league snapshot from the service half (SL-5). */
  snapshot: (league: string) => Promise<Json>;
  news?: FactsDeps['news'];
  classifier?: ClassifierRef;
  now?: () => number;
};

const decisionSchema = obj({
  slots: arr(obj({ slot: str(), playerId: str(), why: str({ maxLength: 240 }) }, { open: true })),
  summary: str({ maxLength: 400 }),
}, { open: true });

const line = (p: PlayerFact) => {
  const g = p.game ? `${p.game.home ? 'vs' : 'at'} ${p.game.opponent}, kickoff ${p.game.kickoff ?? 'unknown'}` : 'no game';
  return `${p.id} ${p.name} (${p.pos}, ${p.team ?? '-'}) proj ${p.proj}${p.inj ? `, ${p.inj}` : ''}, ${g}`;
};

function factsPrompt(f: Facts): string {
  const now = (s: string) => (f.current[s] ? line(f.players[f.current[s]!]) : 'empty');
  const open = f.open.map((s) => `${s} (now ${now(s)})\n${f.candidates[s].map((id) => `  - ${line(f.players[id])}${(f.news[id] ?? []).map((n) => ` ${newsLink(n)}`).join('')}`).join('\n')}`);
  const locked = f.slots.filter((s) => !f.open.includes(s)).map((s) => `${s}: ${now(s)}`);
  const base = f.baseline.map((b) => `${b.slot}: ${b.name} ${b.proj}`);
  return [
    `Week ${f.week}, ${f.team} (${f.record}) against ${f.opponent ?? 'a bye'}.`,
    `Best lineup by projection (the baseline; Out, IR, Doubtful and Sus excluded):\n${base.join('\n')}`,
    `Locked slots, final this week:\n${locked.join('\n') || 'none'}`,
    `Open slots with the players who can fill them:\n${open.join('\n')}`,
    ...(Object.values(f.news).some((n) => n.length) ? ['Linked news is dated. A newer report from a credible source (the team, the league or a named beat reporter) beats the Sleeper designation, which lags; when they conflict, say which you followed.'] : []),
    'Decide every open slot, FLEX included, by player id. Write the summary as "Start X over Y" for each change from the current lineup, or say there are no changes.',
  ].join('\n\n');
}

/** Lineup job (SL-1): facts, one model decision, a sealed card, then the judge's verdict on it. */
export function lineupWorkflow(deps: LineupDeps) {
  const input = (c: StepContext) => JSON.parse(String(c.input)) as LineupInput;
  // close_call_news cannot be met without a news source, so it is scored only once one is wired; the verdict says so.
  const rubric = deps.news ? lineupRubric : { ...lineupRubric, criteria: lineupRubric.criteria.map((c) => ({ ...c, checks: c.checks.filter((k) => k.id !== 'close_call_news') })) };
  const notes = deps.news ? [] : ['close-call news not checked: no news source'];
  const judgeOptions = (f: Facts): JudgeOptions => ({ kind: cardsKind, rubric, evidence: lineupEvidence(f) as never, maxRevisions: 1, classifier: deps.classifier });
  return defineWorkflow({
    name: LINEUP_WORKFLOW,
    steps: [
      {
        name: 'facts',
        kind: 'code',
        run: async (c) => {
          const i = input(c);
          return (await buildFacts({ client: deps.client, news: deps.news }, await deps.snapshot(i.league), i, (deps.now ? deps.now() : c.api.now()))) as unknown as JsonValue;
        },
      },
      {
        name: 'card',
        kind: 'judged',
        schema: decisionSchema,
        prompt: (c) => factsPrompt(c.prior.facts as unknown as Facts),
        card: (c, d) => buildCard(c.prior.facts as unknown as Facts, d as unknown as Decision),
        judge: (c) => ({ options: judgeOptions(c.prior.facts as unknown as Facts), notes }),
      },
    ],
  });
}
