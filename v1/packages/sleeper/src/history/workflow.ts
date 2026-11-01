import type { JsonValue } from '@earendil-works/chord';
import { Type } from '@earendil-works/pi-ai';
import { rejudge, type JudgeOptions, type Verdict } from '@quack/judge';
import { attachVerdict, seal, type SealedArtifact } from '@quack/sdk';
import { defineWorkflow, type StepContext } from '@quack/workflows';
import { cardsKind } from '../lineup/card.ts';
import type { LineupDeps } from '../lineup/workflow.ts';
import { buildHistoryCard, type HistoryDecision } from './card.ts';
import { buildHistoryFacts, type HistoryFacts, type HistoryInput, type SeasonFigures } from './facts.ts';
import { historyEvidence, historyRubric } from './rubric.ts';

export const HISTORY_WORKFLOW = 'sleeper.history';

/** The same wiring as the other jobs; history reads past seasons from the Sleeper client, so the day's snapshot and news are not used. */
export type HistoryDeps = LineupDeps;

const decisionSchema = Type.Object({
  summary: Type.String({ maxLength: 800 }),
  seasons: Type.Array(Type.Object({ season: Type.String(), text: Type.String({ maxLength: 500 }) })),
});

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
    'Write the net-position story across the seasons in `summary`, then at most one note per season in `seasons`: what the manager controlled (lineup execution, the draft) against what they did not (points against, player outcomes). Back each criticism with a pattern across weeks and name the player, pick or week and the number.',
  ].join('\n\n');
}

/** League history job (SL-1): code assembles every figure; one model step writes the narrative; the judge checks the sealed card. */
export function historyWorkflow(deps: HistoryDeps) {
  const input = (c: StepContext) => JSON.parse(String(c.input)) as HistoryInput;
  const judgeOptions = (f: HistoryFacts): JudgeOptions => ({ kind: cardsKind, rubric: historyRubric, evidence: historyEvidence(f) as never, maxRevisions: 1, classifier: deps.classifier });
  return defineWorkflow({
    name: HISTORY_WORKFLOW,
    steps: [
      { name: 'facts', kind: 'code', run: async (c) => (await buildHistoryFacts(deps.client, input(c))) as unknown as JsonValue },
      { name: 'decide', kind: 'model', schema: decisionSchema, prompt: (c) => prompt(c.prior.facts as unknown as HistoryFacts) },
      {
        name: 'card',
        kind: 'code',
        run: async (c) => {
          const card = buildHistoryCard(c.prior.facts as unknown as HistoryFacts, c.prior.decide as unknown as HistoryDecision);
          await c.api.env!.writeFile(cardsKind.path, JSON.stringify(card, null, 1), c.ctx);
          return (await seal(c.api, cardsKind, c.ctx)) as unknown as JsonValue;
        },
      },
      {
        name: 'check',
        kind: 'code',
        run: async (c) => {
          const sealed = c.prior.card as unknown as SealedArtifact;
          const verdict: Verdict = await rejudge(c.api, judgeOptions(c.prior.facts as unknown as HistoryFacts), sealed, `judge:${sealed.kind}:v${sealed.version}`, c.ctx);
          await attachVerdict(c.api, sealed.kind, sealed.version, verdict as never, c.ctx);
          return { passed: verdict.passed, score: verdict.score, gatesFailed: verdict.gatesFailed };
        },
      },
    ],
  });
}
