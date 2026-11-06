import type { DraftFigure, HistoryFacts, SeasonFigures } from './facts.ts';

export type HistoryCard = {
  kind: 'history';
  league: string;
  week: number;
  team: string;
  /** The definitions behind the figures, declared once on the card. */
  definitions: HistoryFacts['definitions'];
  /** Every figure comes from the tools; `note` is the model's read of that season, null when it wrote none, and `cited` the draft rows it discusses. */
  seasons: Array<SeasonFigures & { note: string | null; cited: DraftFigure[] }>;
  summary: string;
};

/** What the model decides: prose keyed by season, and the pick numbers each note discusses, whose figures code puts beside it. */
export type HistoryDecision = { summary: string; seasons: Array<{ season: string; text: string; picks?: number[] }> };

/** A note for a season that was not reviewed, or a pick that season's draft does not have, is an error. */
function citedPicks(s: SeasonFigures, picks: number[]): DraftFigure[] {
  return [...new Set(picks)].map((n) => {
    const row = s.draft?.find((p) => p.pick === n);
    if (!row) throw new Error(`the model cited pick ${n} in season ${s.season}, which ${s.draft ? 'has no such pick' : 'has no draft grade'}`);
    return row;
  });
}

/** The card is the facts plus the model's net-position narrative. */
export function buildHistoryCard(f: HistoryFacts, d: HistoryDecision): HistoryCard {
  const notes = new Map<string, { text: string; cited: DraftFigure[] }>();
  for (const n of d.seasons) {
    const season = f.seasons.find((s) => s.season === n.season);
    if (!season) throw new Error(`the model wrote about season ${n.season}, which was not reviewed`);
    if (notes.has(n.season)) throw new Error(`the model wrote season ${n.season} twice`);
    notes.set(n.season, { text: n.text, cited: citedPicks(season, n.picks ?? []) });
  }
  return { kind: 'history', league: f.league, week: f.week, team: f.team, definitions: f.definitions, seasons: f.seasons.map((s) => ({ ...s, note: notes.get(s.season)?.text ?? null, cited: notes.get(s.season)?.cited ?? [] })), summary: d.summary };
}
