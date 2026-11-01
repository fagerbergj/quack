import type { HistoryFacts, SeasonFigures } from './facts.ts';

export type HistoryCard = {
  kind: 'history';
  league: string;
  week: number;
  team: string;
  /** The definitions behind the figures, declared once on the card. */
  definitions: HistoryFacts['definitions'];
  /** Every figure comes from the tools; `note` is the model's read of that season, null when it wrote none. */
  seasons: Array<SeasonFigures & { note: string | null }>;
  summary: string;
};

/** What the model decides: prose only, keyed by season. */
export type HistoryDecision = { summary: string; seasons: Array<{ season: string; text: string }> };

/** The card is the facts plus the model's net-position narrative; a note for a season that was not reviewed is an error. */
export function buildHistoryCard(f: HistoryFacts, d: HistoryDecision): HistoryCard {
  const notes = new Map<string, string>();
  for (const n of d.seasons) {
    if (!f.seasons.some((s) => s.season === n.season)) throw new Error(`the model wrote about season ${n.season}, which was not reviewed`);
    if (notes.has(n.season)) throw new Error(`the model wrote season ${n.season} twice`);
    notes.set(n.season, n.text);
  }
  return { kind: 'history', league: f.league, week: f.week, team: f.team, definitions: f.definitions, seasons: f.seasons.map((s) => ({ ...s, note: notes.get(s.season) ?? null })), summary: d.summary };
}
