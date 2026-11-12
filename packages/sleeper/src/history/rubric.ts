import type { Declaration, Inputs, Rubric } from '@quack/judge';
import { defect, metaFields, parseCard, SHARE, yes } from '../rubric-kit.ts';
import type { HistoryCard } from './card.ts';
import type { HistoryFacts } from './facts.ts';

const parse = (artifact: string) => parseCard<HistoryCard>(artifact);

const brief = (s: HistoryCard['seasons'][number]) => ({
  season: s.season, record: `${s.wins}-${s.losses}${s.ties ? `-${s.ties}` : ''}`, pf: s.pf, pa: s.pa, pf_rank: s.pf_rank, pa_rank: s.pa_rank, eff: s.eff, left_total: s.left_total, close_losses: s.close_losses, champion: s.champion, in_progress: s.in_progress,
  weeks: s.weeks.map((w) => ({ week: w.week, result: w.result, started: w.started, best: w.best, left: w.left, opp: w.opp })),
  draft: s.draft?.map((p) => ({ round: p.round, pick: p.pick, player: p.player.name, pos: p.player.pos, drafted_as: p.drafted_as, finished: p.finished, pts: p.pts, verdict: p.verdict })) ?? null,
});

/** The judge's named texts: one record per season the model wrote about, with the legacy row unit (that season's figures and the note), and the card unit (the summary, the declared rule and every season's note). */
export function historyInputsOf(_f: HistoryFacts, content: string): Inputs {
  const card = parse(content);
  const rows = card.seasons.filter((s) => s.note).map((s) => ({ id: `season${s.season}`, season: s.season, note: s.note, legacy: { season: brief(s), note: s.note } }));
  const legacy = { summary: card.summary, definitions: card.definitions, seasons: card.seasons.map((s) => ({ season: s.season, record: brief(s).record, eff: s.eff, note: s.note })) };
  return { seasons: JSON.stringify(rows), card: JSON.stringify([{ id: 'card', legacy }]) };
}
export const historyInputs = (f: HistoryFacts) => async (_api: unknown, sealed: { content: string }): Promise<Inputs> => historyInputsOf(f, sealed.content);

/** How each model check of historyRubric is asked: of the card or per season's note, each item alone with nothing shared. */
export const historyDeclaration: Declaration = {
  scopes: {
    card: { split: { name: 'sleeper.legacy', from: 'card', path: 'summary' }, piece: 'summary', context: metaFields(['definitions', 'seasons']) },
    notes: { split: { name: 'jsonItems', from: 'seasons', pick: ['season', 'note'] }, piece: 'row' },
    row: { split: { name: 'sleeper.legacy', from: 'seasons', path: 'season' }, piece: 'season', context: metaFields(['note']) },
  },
  checks: {
    luck_process_split: { scope: 'card', aggregate: SHARE, question: yes('Does it separate lineup execution and the draft, which the manager controls, from schedule luck (points against) and player-outcome luck?') },
    // Weighing each criticism against the season's figures is a claim against evidence, which the decision model passes unread, so the 27B decides it.
    aggregated_not_scolded: { scope: 'row', aggregate: SHARE, decisionModel: false, question: yes('Does every criticism of the manager in this note cite a number from this season\'s figures that backs it? A note with no criticism meets this.') },
    decision_time_grading: { scope: 'notes', aggregate: SHARE, question: yes('Is a pre-kickoff call judged on what was knowable then, not on how the game turned out?') },
    single_week_as_pattern: { scope: 'notes', aggregate: SHARE, question: defect('Is one week, one bust or one trade offered as evidence of a bad manager?') },
    // Clef passed a summary that says efficiency decides the record on replay, so the 27B decides it on the evidence it had before the port.
    efficiency_as_record: { scope: 'card', aggregate: SHARE, decisionModel: false, question: defect('Does it equate lineup efficiency with winning or losing?') },
  },
};

// Every figure, the declared rule and each cited pick's ranks are copied onto the card by code, so no figure is checked in prose.
// History weights are unreplayed: replay them before any weight gates delivery.
export const historyRubric: Rubric = {
  name: 'sleeper.history',
  version: 5,
  threshold: 0.8,
  criteria: [
    { id: 'net_position_is_reasoned', checks: [
      { id: 'luck_process_split', label: "Mixes luck up with decisions", weight: 3 },
      { id: 'aggregated_not_scolded', label: "Criticizes without the season's figures behind it", weight: 3 },
      { id: 'decision_time_grading', label: "Judges a call by how the game ended", weight: 2 },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'single_week_as_pattern', label: "Treats one bad week as a pattern", weight: -3 },
      { id: 'efficiency_as_record', label: "Confuses lineup efficiency with winning", weight: -2 },
    ] },
  ],
};
