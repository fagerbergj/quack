import { arr, int, obj, opt, str } from '@earendil-works/pi-schema';
import { defineWorkflow } from '@quack/workflows';
import { cardSteps } from '../job.ts';
import type { LineupDeps } from '../lineup/workflow.ts';
import { buildHistoryCard, type HistoryDecision } from './card.ts';
import { buildHistoryFacts, type HistoryFacts, type SeasonFigures } from './facts.ts';
import { historyDeclaration, historyInputs, historyRubric } from './rubric.ts';

export const HISTORY_WORKFLOW = 'sleeper.history';

/** The same wiring as the other jobs; history reads past seasons from the Sleeper client, so the day's snapshot and news are not used. */
export type HistoryDeps = LineupDeps;

const decisionSchema = obj({
  summary: str({ maxLength: 800 }),
  seasons: arr(obj({ season: str(), text: str({ maxLength: 500 }), picks: opt(arr(int({ minimum: 1 }))) }, { open: true })),
}, { open: true });

const seasonText = (s: SeasonFigures) => {
  const worst = [...s.weeks].sort((a, b) => b.left - a.left).slice(0, 3).map((w) => `wk ${w.week} left ${w.left}`).join(', ');
  const picks = (s.draft ?? []).map((p) => `    - round ${p.round} pick ${p.pick} ${p.player.name} (${p.player.pos}): drafted as ${p.player.pos}${p.drafted_as}, finished ${p.player.pos}${p.finished}, ${p.pts} pts, ${p.verdict}`);
  return [
    `${s.season}${s.in_progress ? ' (in progress)' : ''}: ${s.wins}-${s.losses}${s.ties ? `-${s.ties}` : ''}, points for ${s.pf} (rank ${s.pf_rank}), against ${s.pa} (rank ${s.pa_rank}; 1 is fewest), efficiency ${s.eff ?? 'unknown'}%, ${s.left_total} left on the bench over ${s.weeks.length} weeks (largest: ${worst || 'none'}), ${s.close_losses} close losses, champion ${s.champion ?? 'unknown'}.`,
    ...(s.draft ? [`  Draft picks:`, ...picks] : ['  No draft grade: the season is not over.']),
  ].join('\n');
};

function prompt(f: HistoryFacts): string {
  return [
    `League history for ${f.team}, newest season first. Every figure below is computed; do not recompute or restate one differently.`,
    `Definitions in use:\n- ${Object.values(f.definitions).join('\n- ')}`,
    f.seasons.map(seasonText).join('\n\n'),
    'Write the net-position story across the seasons in `summary`, then at most one note per season in `seasons`: what the manager controlled (lineup execution, the draft) against what they did not (points against, player outcomes). Back each criticism with a number from this season\'s figures and name the player or week it is about; one bad week, one bust or one trade is not a verdict on the manager. Never write a pick number or a draft or finish rank in `text`: list the pick numbers a note discusses in `picks`, and the card shows their figures beside it.',
  ].join('\n\n');
}

/** League history job: code assembles every figure; one model step writes the narrative; the judge checks the sealed card. */
export function historyWorkflow(deps: HistoryDeps) {
  return defineWorkflow({
    name: HISTORY_WORKFLOW,
    steps: cardSteps<HistoryFacts, HistoryDecision>(deps, {
      facts: (_c, i) => buildHistoryFacts(deps.client, i),
      schema: decisionSchema, prompt, card: buildHistoryCard, rubric: historyRubric, declaration: historyDeclaration, inputs: historyInputs,
    }),
  });
}
