import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import type { HistoryCard } from './card.ts';
import { verdictOf, type HistoryFacts, type SeasonFigures } from './facts.ts';

const parse = (artifact: string) => JSON.parse(artifact) as HistoryCard;
const factsOf = (ev: Evidence) => ev.facts[0].state as unknown as HistoryFacts;
const withoutNote = ({ note: _note, ...figures }: HistoryCard['seasons'][number]) => figures;

const CLAUSE_END = /[;)]|\.(?:\s|$)/;
const lastName = (name: string) => name.split(' ').at(-1)!;
const escape = (t: string) => t.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

/** A pick or rank quoted in a note right after a drafted player's name ("Flowers pick 54", "Kupp WR3→32") that is not his. */
export function misquotedPicks(note: string, draft: SeasonFigures['draft']): string[] {
  const rows = draft ?? [];
  const named = rows.map((p) => ({ p, re: new RegExp(`\\b(?:${escape(p.player.name)}${rows.filter((q) => lastName(q.player.name) === lastName(p.player.name)).length === 1 ? `|${escape(lastName(p.player.name))}` : ''})\\b`, 'g') }));
  return named.flatMap(({ p, re }) => [...note.matchAll(re)].flatMap((m) => {
    // The clause after the name, up to its close or the next drafted name, holds that player's figures.
    const at = m.index! + m[0].length;
    const rest = note.slice(at, at + 60);
    const stops = [rest.search(CLAUSE_END), ...named.filter((o) => o.p !== p).map((o) => rest.search(new RegExp(o.re.source)))].filter((i) => i >= 0);
    const clause = rest.slice(0, Math.min(60, ...stops));
    const pick = clause.match(/\bpick\s+(\d+)/i);
    const ranks = clause.match(/\b[A-Z]{0,3}(\d+)\s*→\s*[A-Z]{0,3}(\d+)/);
    return [
      ...(pick && Number(pick[1]) !== p.pick ? [`${m[0]} pick ${pick[1]} (the draft has ${p.pick})`] : []),
      ...(ranks && (Number(ranks[1]) !== p.drafted_as || Number(ranks[2]) !== p.finished) ? [`${m[0]} ${ranks[1]}→${ranks[2]} (the draft has ${p.drafted_as}→${p.finished})`] : []),
    ];
  }));
}

// Every figure is compared with the one the tools produced, a pick or rank a note quotes for a drafted player too,
// and each verdict is recomputed from its ranks under the single declared rule.
function figuresFromTools(artifact: string, ev: Evidence): CodeResult {
  const c = parse(artifact);
  const f = factsOf(ev);
  const misquoted = new Map(c.seasons.map((x) => [x.season, x.note ? misquotedPicks(x.note, x.draft) : []]));
  const bad = f.seasons.filter((s) => {
    const got = c.seasons.find((x) => x.season === s.season);
    return !got || JSON.stringify(withoutNote(got)) !== JSON.stringify(s) || (got.draft ?? []).some((p) => p.verdict !== verdictOf(p.drafted_as, p.finished)) || misquoted.get(s.season)!.length > 0;
  });
  const quotes = [...misquoted].flatMap(([season, m]) => (m.length ? [`${season} note quotes ${m.join(', ')}`] : []));
  const wrongRule = JSON.stringify(c.definitions) !== JSON.stringify(f.definitions) || c.seasons.length !== f.seasons.length;
  const met = f.seasons.length ? (f.seasons.length - bad.length) / f.seasons.length : 1;
  return { met: wrongRule ? Math.min(met, 0) : met, detail: bad.length || wrongRule ? [`season ${bad.map((s) => s.season).join(', ') || '-'} differ from the tools${wrongRule ? '; the declared definitions or season list changed' : ''}`, ...quotes].join('; ') : undefined };
}

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

// The audit's history table (docs/v1/audits/sleeper.md), unreplayed: D45 asks for replay before any weight gates delivery.
export const historyRubric: Rubric = {
  name: 'sleeper.history',
  version: 3,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [{ id: 'figures_from_tools', label: "Numbers differ from the league data", weight: 'gate', code: figuresFromTools }] },
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
