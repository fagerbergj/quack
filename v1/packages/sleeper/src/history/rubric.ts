import type { Evidence, Rubric } from '@quack/judge';
import { parseCard } from '../rubric-kit.ts';
import type { HistoryCard } from './card.ts';
import type { HistoryFacts } from './facts.ts';

const parse = (artifact: string) => parseCard<HistoryCard>(artifact);

const brief = (s: HistoryCard['seasons'][number]) => ({
  season: s.season, record: `${s.wins}-${s.losses}${s.ties ? `-${s.ties}` : ''}`, pf: s.pf, pa: s.pa, pf_rank: s.pf_rank, pa_rank: s.pa_rank, eff: s.eff, left_total: s.left_total, close_losses: s.close_losses, champion: s.champion, in_progress: s.in_progress,
  weeks: s.weeks.map((w) => ({ week: w.week, result: w.result, started: w.started, best: w.best, left: w.left, opp: w.opp })),
  draft: s.draft?.map((p) => ({ round: p.round, pick: p.pick, player: p.player.name, pos: p.player.pos, drafted_as: p.drafted_as, finished: p.finished, pts: p.pts, verdict: p.verdict })) ?? null,
});

/** One unit per season the model wrote about, carrying that season's figures; the card unit holds the summary and the declared rule. */
export function historyEvidence(f: HistoryFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse(sealed.content);
    return {
      facts: [{ id: 'facts', whole: true as const, state: f as never }],
      row: card.seasons.filter((s) => s.note).map((s) => ({ id: `season${s.season}`, state: { season: brief(s), note: s.note } as never })),
      card: [{ id: 'card', whole: true as const, state: { summary: card.summary, definitions: card.definitions, seasons: card.seasons.map((s) => ({ season: s.season, record: brief(s).record, eff: s.eff, note: s.note })) } as never }],
    };
  };
}

// Every figure, the declared rule and each cited pick's ranks are copied onto the card by code, so no figure is checked in prose.
export const historyRubric: Rubric = {
  name: 'sleeper.history',
  version: 4,
  threshold: 0.8,
  criteria: [
    { id: 'net_position_is_reasoned', checks: [
      { id: 'luck_process_split', label: "Mixes luck up with decisions", weight: 3, scope: 'card', question: 'Does it separate lineup execution and the draft, which the manager controls, from schedule luck (points against) and player-outcome luck?' },
      { id: 'aggregated_not_scolded', label: "Criticizes without the season's figures behind it", weight: 3, scope: 'row', question: 'Does every criticism of the manager in this note cite a number from this season\'s figures that backs it? A note with no criticism meets this.' },
      { id: 'decision_time_grading', label: "Judges a call by how the game ended", weight: 2, scope: 'row', question: 'Is a pre-kickoff call judged on what was knowable then, not on how the game turned out?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'single_week_as_pattern', label: "Treats one bad week as a pattern", weight: -3, scope: 'row', question: 'Is one week, one bust or one trade offered as evidence of a bad manager?' },
      { id: 'efficiency_as_record', label: "Confuses lineup efficiency with winning", weight: -2, scope: 'card', question: 'Does it equate lineup efficiency with winning or losing?' },
    ] },
  ],
};
