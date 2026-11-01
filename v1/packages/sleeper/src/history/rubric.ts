import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import type { HistoryCard } from './card.ts';
import { verdictOf, type HistoryFacts } from './facts.ts';

const parse = (artifact: string) => JSON.parse(artifact) as HistoryCard;
const factsOf = (ev: Evidence) => ev.facts[0].state as unknown as HistoryFacts;
const withoutNote = ({ note: _note, ...figures }: HistoryCard['seasons'][number]) => figures;

// Every figure is compared with the one the tools produced, and each verdict is recomputed from its ranks under the single declared rule.
function figuresFromTools(artifact: string, ev: Evidence): CodeResult {
  const c = parse(artifact);
  const f = factsOf(ev);
  const bad = f.seasons.filter((s) => {
    const got = c.seasons.find((x) => x.season === s.season);
    return !got || JSON.stringify(withoutNote(got)) !== JSON.stringify(s) || (got.draft ?? []).some((p) => p.verdict !== verdictOf(p.drafted_as, p.finished));
  });
  const wrongRule = JSON.stringify(c.definitions) !== JSON.stringify(f.definitions) || c.seasons.length !== f.seasons.length;
  const met = f.seasons.length ? (f.seasons.length - bad.length) / f.seasons.length : 1;
  return { met: wrongRule ? Math.min(met, 0) : met, detail: bad.length || wrongRule ? `season ${bad.map((s) => s.season).join(', ') || '-'} differ from the tools${wrongRule ? '; the declared definitions or season list changed' : ''}` : undefined };
}

const brief = (s: HistoryCard['seasons'][number]) => ({
  season: s.season, record: `${s.wins}-${s.losses}${s.ties ? `-${s.ties}` : ''}`, pf: s.pf, pa: s.pa, pf_rank: s.pf_rank, pa_rank: s.pa_rank, eff: s.eff, left_total: s.left_total, close_losses: s.close_losses, champion: s.champion, in_progress: s.in_progress,
  weeks: s.weeks.map((w) => ({ week: w.week, result: w.result, started: w.started, best: w.best, left: w.left, opp: w.opp })),
  draft: s.draft?.map((p) => ({ round: p.round, pick: p.pick, player: p.player.name, pos: p.player.pos, drafted_as: p.drafted_as, finished: p.finished, verdict: p.verdict })) ?? null,
});

/** One unit per season the model wrote about, carrying that season's figures; the card unit holds the summary and the declared rule. */
export function historyEvidence(f: HistoryFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse(sealed.content);
    return {
      facts: [{ id: 'facts', state: f as never }],
      row: card.seasons.filter((s) => s.note).map((s) => ({ id: `season${s.season}`, state: { season: brief(s), note: s.note } as never })),
      card: [{ id: 'card', state: { summary: card.summary, definitions: card.definitions, seasons: card.seasons.map((s) => ({ season: s.season, record: brief(s).record, eff: s.eff, note: s.note })) } as never }],
    };
  };
}

// The audit's history table (docs/v1/audits/sleeper.md), unreplayed: D45 asks for replay before any weight gates delivery.
export const historyRubric: Rubric = {
  name: 'sleeper.history',
  version: 1,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [{ id: 'figures_from_tools', weight: 'gate', code: figuresFromTools }] },
    { id: 'net_position_is_reasoned', checks: [
      { id: 'luck_process_split', weight: 3, scope: 'card', question: 'Does it separate lineup execution and the draft, which the manager controls, from schedule luck (points against) and player-outcome luck?' },
      { id: 'aggregated_not_scolded', weight: 3, scope: 'row', question: 'Is each criticism backed by a pattern across weeks, naming the player, pick or week and the number from this season\'s figures?' },
      { id: 'decision_time_grading', weight: 2, scope: 'row', question: 'Is a pre-kickoff call judged on what was knowable then, not on how the game turned out?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'single_week_as_pattern', weight: -3, scope: 'row', question: 'Is one week, one bust or one trade offered as evidence of a bad manager?' },
      { id: 'efficiency_as_record', weight: -2, scope: 'card', question: 'Does it equate lineup efficiency with winning or losing?' },
    ] },
  ],
};
