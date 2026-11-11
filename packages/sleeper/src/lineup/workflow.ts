import { arr, obj, str } from '@earendil-works/pi-schema';
import type { DecisionModelRef } from '@quack/judge';
import { defineWorkflow } from '@quack/workflows';
import { newsLink } from './news.ts';
import type { SleeperClient } from '../client.ts';
import { liveLeague } from '../snapshot.ts';
import { buildCard, type Decision } from './card.ts';
import { buildFacts, type Facts, type FactsDeps, type PlayerFact } from './facts.ts';
import { lineupDeclaration, lineupInputs, lineupRubric } from './rubric.ts';
import { cardSteps } from '../job.ts';

export const LINEUP_WORKFLOW = 'sleeper.lineup';

export type LineupDeps = {
  client: SleeperClient;
  news?: FactsDeps['news'];
  now?: () => number;
  /** A decision model such as Clef, asked the judge's local questions first. */
  decisionModel?: DecisionModelRef;
};

const decisionSchema = obj({
  slots: arr(obj({ slot: str(), playerId: str(), why: str({ maxLength: 240 }) }, { open: true })),
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
    'Decide every open slot, FLEX included, by player id. The card lists the changes itself; `why` says what decided each slot.',
  ].join('\n\n');
}

/** Lineup job: facts, one model decision, a sealed card, then the judge's verdict on it. */
export function lineupWorkflow(deps: LineupDeps) {
  return defineWorkflow({
    name: LINEUP_WORKFLOW,
    steps: cardSteps<Facts, Decision>(deps, {
      facts: async (_c, i, at) => buildFacts({ client: deps.client, news: deps.news }, await liveLeague(deps.client, i.league), i, at),
      // Without a news source no row carries news, so the news check has no item and is not scored.
      schema: decisionSchema, prompt: factsPrompt, card: buildCard, rubric: lineupRubric, declaration: lineupDeclaration, inputs: lineupInputs,
    }),
  });
}
