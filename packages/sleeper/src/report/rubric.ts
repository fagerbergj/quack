import { registerSplitter, type Declaration, type Inputs, type Rubric } from '@quack/judge';
import { defect, metaFields, parseCard, SHARE, yes } from '../rubric-kit.ts';
import type { DigestCard, DigestFacts } from './digest.ts';
import type { RetroCard, RetroFacts } from './retro.ts';

const parse = <C>(artifact: string) => parseCard<C>(artifact);
type Sealed = { content: string };

/** Each game as one record: what the flow check reads, and the legacy unit preview_news_decides keeps. */
function digestGames(f: DigestFacts, card: DigestCard) {
  return card.games.map((g) => ({
    id: `game${g.id}`, home: `${g.home.team} ${g.home.pts}`, away: `${g.away.team} ${g.away.pts}`, margin: g.margin, note: g.note,
    legacy: { game: g, mode: f.mode, news: g.news, designations: Object.fromEntries(f.games.find((x) => x.id === g.id)?.sides.flatMap((s) => s.designated.map((d) => [d.name, d.inj])) ?? []) },
  }));
}

/** The judge's named texts: the card's games, and the legacy card unit (the summary, every game's note and the standings). */
export function digestInputsOf(f: DigestFacts, content: string): Inputs {
  const card = parse<DigestCard>(content);
  const legacy = { summary: card.summary, mode: f.mode, week: f.week, games: card.games.map((g) => ({ home: g.home.team, away: g.away.team, margin: g.margin, note: g.note })), standings: f.standings };
  return { games: JSON.stringify(digestGames(f, card)), card: JSON.stringify([{ id: 'card', legacy }]) };
}
export const digestInputs = (f: DigestFacts) => async (_api: unknown, sealed: Sealed): Promise<Inputs> => digestInputsOf(f, sealed.content);

// A check that stays on the 27B reads its legacy unit: the record's `legacy[path]` as the piece, the rest of `legacy` around it.
registerSplitter('sleeper.reportLegacy', (inputs, o) => (JSON.parse(inputs[String(o.from)]) as Array<{ id: string; legacy: Record<string, unknown> }>).map((r) => ({ id: r.id, text: JSON.stringify(r.legacy[String(o.path)]), meta: r.legacy })));

/** How each model check of digestRubric is asked: of the card or per game, each item alone with nothing shared. */
export const digestDeclaration: Declaration = {
  scopes: {
    card: { split: { name: 'sleeper.reportLegacy', from: 'card', path: 'summary' }, piece: 'summary', context: metaFields(['mode', 'week', 'games', 'standings']) },
    games: { split: { name: 'jsonItems', from: 'games', pick: ['home', 'away', 'margin', 'note'] }, piece: 'game' },
    // News is searched only for a preview, so with no source no game has any and the check is not scored.
    preview: { split: { name: 'sleeper.reportLegacy', from: 'games', path: 'game', where: [{ path: 'news', op: 'truthy' }] }, piece: 'game', context: metaFields(['mode', 'news', 'designations']) },
  },
  checks: {
    // Clef was unsure on every item of this and small_sample_labeled on replay, so the 27B decides them on the evidence it had before the port.
    story_in_summary: { scope: 'card', aggregate: SHARE, decisionModel: false, question: yes('Does the summary name the week\'s real story (the margin that mattered, the high score, the blowout) rather than recite scores?') },
    // Weighing the note against the reports is a claim against evidence, which the decision model passes unread, so the 27B decides it.
    preview_news_decides: { scope: 'preview', aggregate: SHARE, decisionModel: false, question: yes('Does this game\'s note cite the attached `news` or an injury designation that swings it, naming the right player for each report?') },
    small_sample_labeled: { scope: 'card', aggregate: SHARE, decisionModel: false, question: defect('Does a claim drawn from under three weeks of data go unlabeled as early?') },
    luck_without_margin: { scope: 'card', aggregate: SHARE, question: defect('Does `summary` or a game\'s `note` call a result lucky without its point margin, or from fewer than about eight close games?') },
    game_flow_from_score: { scope: 'games', aggregate: SHARE, question: defect('Does `note` describe how the game unfolded, such as a comeback, a collapse or a lead that changed hands, which the final score alone does not show?') },
  },
};

// The card builder writes one row per game with its scores, margin and top scorers from the facts, and the model never sees a projection.
// Digest weights are unreplayed: replay them before any weight gates delivery.
export const digestRubric: Rubric = {
  name: 'sleeper.digest',
  version: 4,
  threshold: 0.8,
  criteria: [
    { id: 'tells_the_week', checks: [
      { id: 'story_in_summary', label: "Summary just recites scores", weight: 3 },
      { id: 'preview_news_decides', label: "A preview ignores news that swings it", weight: 3 },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'small_sample_labeled', label: "Draws a trend from too few weeks", weight: -2 },
      { id: 'luck_without_margin', label: "Calls a result lucky without the margin", weight: -3 },
      { id: 'game_flow_from_score', label: "Invents how a game unfolded", weight: -3 },
    ] },
  ],
};

/** The judge's named texts: the graded rows, the lessons, and the legacy card unit (summary, lessons, result and every row's swing). */
export function retroInputsOf(_f: RetroFacts, content: string): Inputs {
  const card = parse<RetroCard>(content);
  const graded = [...card.misses, ...card.waiver_misses];
  const rows = graded.map((r, i) => ({ id: `row${i + 1}`, knowable: r.knowable, signal: r.signal, why: r.why }));
  const legacy = { summary: card.summary, lessons: card.lessons, won: card.won, left: card.left, swings: graded.map((r) => ('swing' in r ? r.swing : r.pts - r.over_pts)) };
  return { rows: JSON.stringify(rows), lessons: JSON.stringify(card.lessons), card: JSON.stringify([{ id: 'card', legacy }]) };
}
export const retroInputs = (f: RetroFacts) => async (_api: unknown, sealed: Sealed): Promise<Inputs> => retroInputsOf(f, sealed.content);

/** How each model check of retroRubric is asked: per row, per lesson or of the summary, each item alone with nothing shared. */
export const retroDeclaration: Declaration = {
  scopes: {
    // A row is knowable only with an attached signal, so with no news source none is and the check is not scored.
    known: { split: { name: 'jsonItems', from: 'rows', pick: ['signal', 'why'], where: [{ path: 'knowable', op: 'truthy' }] }, piece: 'row' },
    variance: { split: { name: 'jsonItems', from: 'rows', pick: ['why'], where: [{ path: 'knowable', op: 'falsy' }] }, piece: 'row' },
    lessons: { split: { name: 'jsonItems', from: 'lessons' }, piece: 'lesson' },
    card: { split: { name: 'sleeper.reportLegacy', from: 'card', path: 'summary' }, piece: 'summary', context: metaFields(['lessons', 'won', 'left', 'swings']) },
  },
  checks: {
    signal_is_evidence: { scope: 'known', aggregate: SHARE, question: yes('Is `signal` a dated news item or usage trend, not the player\'s injury designation restated?') },
    lessons_actionable: { scope: 'lessons', aggregate: SHARE, question: yes('Is this lesson something the manager can do next week?') },
    // Clef passed a summary that names a smaller swing on replay, so the 27B decides it on the evidence it had before the port.
    summary_names_swing: { scope: 'card', aggregate: SHARE, decisionModel: false, question: yes('Does the summary name the single biggest swing among the `swings`?') },
    hindsight_called_process: { scope: 'variance', aggregate: SHARE, question: defect('This miss had no signal before kickoff. Does `why` blame the manager\'s process for it?') },
  },
};

// The card builder copies every figure and marks a row knowable only with an attached signal dated before its cutoff.
// Retro weights are unreplayed: replay them before any weight gates delivery.
export const retroRubric: Rubric = {
  name: 'sleeper.retro',
  version: 4,
  threshold: 0.8,
  criteria: [
    { id: 'grades_the_week', checks: [
      { id: 'signal_is_evidence', label: "Signal is not a dated news item", weight: 3 },
      { id: 'lessons_actionable', label: "Lessons can't be acted on next week", weight: 2 },
      { id: 'summary_names_swing', label: "Summary misses the biggest swing", weight: 2 },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'hindsight_called_process', label: "Blames the manager for bad luck", weight: -4 },
    ] },
  ],
};
