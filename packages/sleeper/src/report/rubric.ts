import type { Evidence, Rubric } from '@quack/judge';
import { parseCard, type NewsGate } from '../rubric-kit.ts';
import type { DigestCard, DigestFacts } from './digest.ts';
import type { RetroCard, RetroFacts } from './retro.ts';

const parse = <C>(artifact: string) => parseCard<C>(artifact);

/** Row units hold the game, its news and the designations of the starters the news was searched for. */
export function digestEvidence(f: DigestFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse<DigestCard>(sealed.content);
    const rows = card.games.map((g) => ({ id: `game${g.id}`, state: { game: g, mode: f.mode, news: g.news, designations: Object.fromEntries(f.games.find((x) => x.id === g.id)?.sides.flatMap((s) => s.designated.map((d) => [d.name, d.inj])) ?? []) } as never }));
    return {
      facts: [{ id: 'facts', whole: true as const, state: f as never }],
      row: rows,
      preview: f.mode === 'preview' ? rows : [],
      card: [{ id: 'card', whole: true as const, state: { summary: card.summary, mode: f.mode, week: f.week, games: card.games.map((g) => ({ home: g.home.team, away: g.away.team, margin: g.margin, note: g.note })), standings: f.standings } as never }],
    };
  };
}

// The card builder writes one row per game with its scores, margin and top scorers from the facts, and the model never sees a projection.
// Digest weights are unreplayed: replay them before any weight gates delivery.
export const digestRubric: Rubric = {
  name: 'sleeper.digest',
  version: 3,
  threshold: 0.8,
  criteria: [
    { id: 'tells_the_week', checks: [
      { id: 'story_in_summary', label: "Summary just recites scores", weight: 3, scope: 'card', question: 'Does the summary name the week\'s real story (the margin that mattered, the high score, the blowout) rather than recite scores?' },
      { id: 'preview_news_decides', label: "A preview ignores news that swings it", weight: 3, scope: 'preview', question: 'Does this game\'s note cite the attached `news` or an injury designation that swings it, naming the right player for each report?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'small_sample_labeled', label: "Draws a trend from too few weeks", weight: -2, scope: 'card', question: 'Does a claim drawn from under three weeks of data go unlabeled as early?' },
      { id: 'luck_without_margin', label: "Calls a result lucky without the margin", weight: -3, scope: 'card', question: 'Does it call a result lucky without the point margin, or from fewer than about eight close games?' },
      { id: 'game_flow_from_score', label: "Invents how a game unfolded", weight: -3, scope: 'row', question: 'Does it describe a comeback, collapse or other game flow that only the final score implies?' },
    ] },
  ],
};

type Graded = RetroCard['misses'][number] | RetroCard['waiver_misses'][number];
const graded = (c: RetroCard): Graded[] => [...c.misses, ...c.waiver_misses];

/** Row units hold the graded miss and its news; `known` holds the rows that claim a pre-kickoff signal. */
export function retroEvidence(f: RetroFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse<RetroCard>(sealed.content);
    const news = [...f.misses, ...f.waiverMisses].map((r) => r.news);
    const unit = (r: Graded, i: number) => ({ id: `row${i + 1}`, state: { row: r, news: news[i] } as never });
    const rows = graded(card).map(unit);
    const cardUnit = { id: 'card', whole: true as const, state: { summary: card.summary, lessons: card.lessons, won: card.won, left: card.left, swings: graded(card).map((r) => ('swing' in r ? r.swing : r.pts - r.over_pts)) } as never };
    return {
      facts: [{ id: 'facts', whole: true as const, state: f as never }],
      row: rows,
      known: graded(card).flatMap((r, i) => (r.knowable ? [rows[i]] : [])),
      card: [cardUnit],
      // A card with no lessons has none to judge as actionable.
      lessons: card.lessons.length ? [cardUnit] : [],
    };
  };
}

// The card builder copies every figure and marks a row knowable only with an attached signal dated before its cutoff.
// Retro weights are unreplayed: replay them before any weight gates delivery.
export const retroRubric: Rubric = {
  name: 'sleeper.retro',
  version: 3,
  threshold: 0.8,
  criteria: [
    { id: 'grades_the_week', checks: [
      { id: 'signal_is_evidence', label: "Signal is not a dated news item", weight: 3, scope: 'known', question: 'Is the signal a dated news item or usage trend from the attached `news`, not the same injury designation restated?' },
      { id: 'lessons_actionable', label: "Lessons can't be acted on next week", weight: 2, scope: 'lessons', question: 'Are the lessons (at most three) things the manager can do next week?' },
      { id: 'summary_names_swing', label: "Summary misses the biggest swing", weight: 2, scope: 'card', question: 'Does the summary name the single biggest swing among the `swings`?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'hindsight_called_process', label: "Blames the manager for bad luck", weight: -4, scope: 'row', question: 'Is a row with knowable false (variance, no signal before kickoff) blamed on the manager\'s process?' },
    ] },
  ],
};

/** The report jobs' news checks, gated by the live judge and its replay alike. */
export const DIGEST_NEWS: NewsGate = { checks: ['preview_news_decides'], what: 'preview_news_decides' };
export const RETRO_NEWS: NewsGate = { checks: ['signal_is_evidence'], what: 'signal_is_evidence' };
