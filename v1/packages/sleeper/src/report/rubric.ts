import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import type { DigestCard, DigestFacts } from './digest.ts';
import type { RetroCard, RetroFacts } from './retro.ts';

const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });
const factsOf = <F>(ev: Evidence) => ev.facts[0].state as unknown as F;
const parse = <C>(artifact: string) => JSON.parse(artifact) as C;
const same = (a: number | null, b: number | null) => a === b || (a !== null && b !== null && Math.abs(a - b) < 0.005);

// A preview that names a projection or a chance is the week-8 failure (#1816); the playoff line comes from the standings table only.
const PROJECTION = /\bprojected (?:points?|total|score|winner|loser|to (?:win|lose|beat)|\d)|\bprojections?\b|\bprobabilit|\bodds\b|chances? (?:to|of) (?:win|mak)|\d\s?% (?:chance|probab)/i;

function everyGameCovered(artifact: string, ev: Evidence): CodeResult {
  const have = new Set(parse<DigestCard>(artifact).games.map((g) => g.id));
  const missing = factsOf<DigestFacts>(ev).games.filter((g) => !have.has(g.id));
  const all = factsOf<DigestFacts>(ev).games.length;
  return share(all - missing.length, all, `no row for ${missing.map((g) => `${g.sides[0].team} v ${g.sides[1].team}`).join(', ')}`);
}

const scorers = (list: Array<{ name: string; pts: number }>) => list.map((s) => `${s.name}:${s.pts}`).join(',');

function numbersMatchTools(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf<DigestFacts>(ev);
  const cards = new Map(parse<DigestCard>(artifact).games.map((g) => [g.id, g]));
  const wrong = f.games.filter((g) => {
    const c = cards.get(g.id);
    const [h, a] = g.sides;
    return !c || !same(c.home.pts, h.pts) || !same(c.away.pts, a.pts) || !same(c.margin, g.margin) || scorers(c.home.top_scorers) !== scorers(h.top) || scorers(c.away.top_scorers) !== scorers(a.top);
  });
  return share(f.games.length - wrong.length, f.games.length, `score, margin or top scorers differ from the tools for ${wrong.map((g) => `${g.sides[0].team} v ${g.sides[1].team}`).join(', ')}`);
}

function noProjectionSource(artifact: string): CodeResult {
  const c = parse<DigestCard>(artifact);
  const hit = [c.summary, c.source_note, ...c.games.map((g) => g.note)].find((t) => PROJECTION.test(t));
  return { met: hit ? 0 : 1, detail: hit ? `projection or win-probability language: ${hit.match(PROJECTION)![0]}` : undefined };
}

/** Row units hold the game, its news and the designations of the starters the news was searched for. */
export function digestEvidence(f: DigestFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse<DigestCard>(sealed.content);
    const rows = card.games.map((g) => ({ id: `game${g.id}`, state: { game: g, mode: f.mode, news: g.news, designations: Object.fromEntries(f.games.find((x) => x.id === g.id)?.sides.flatMap((s) => s.designated.map((d) => [d.name, d.inj])) ?? []) } as never }));
    return {
      facts: [{ id: 'facts', state: f as never }],
      row: rows,
      preview: f.mode === 'preview' ? rows : [],
      card: [{ id: 'card', state: { summary: card.summary, mode: f.mode, week: f.week, games: card.games.map((g) => ({ home: g.home.team, away: g.away.team, margin: g.margin, note: g.note })), standings: f.standings } as never }],
    };
  };
}

// The audit's digest table (docs/v1/audits/sleeper.md), unreplayed: D45 asks for replay before any weight gates delivery.
export const digestRubric: Rubric = {
  name: 'sleeper.digest',
  version: 1,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [
      { id: 'every_game_covered', weight: 'gate', code: everyGameCovered },
      { id: 'numbers_match_tools', weight: 'gate', code: numbersMatchTools },
      { id: 'no_projection_source', weight: 'gate', code: noProjectionSource },
    ] },
    { id: 'tells_the_week', checks: [
      { id: 'story_in_summary', weight: 3, scope: 'card', question: 'Does the summary name the week\'s real story (the margin that mattered, the high score, the blowout) rather than recite scores?' },
      { id: 'preview_news_decides', weight: 3, scope: 'preview', question: 'Does this game\'s note cite the attached `news` or an injury designation that swings it, naming the right player for each report?' },
      { id: 'small_sample_labeled', weight: 2, scope: 'card', question: 'Are claims drawn from under three weeks of data labeled as such?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'luck_without_margin', weight: -3, scope: 'card', question: 'Does it call a result lucky without the point margin, or from fewer than about eight close games?' },
      { id: 'game_flow_from_score', weight: -3, scope: 'row', question: 'Does it describe a comeback, collapse or other game flow that only the final score implies?' },
    ] },
  ],
};

function toolFiguresCopied(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf<RetroFacts>(ev);
  const c = parse<RetroCard>(artifact);
  const wrong = [
    ...(['started', 'opp', 'best', 'left'] as const).filter((k) => !same(c[k], f[k])),
    ...(c.won === f.won ? [] : ['won']),
    ...(c.misses.length === f.misses.length && f.misses.every((m, i) => c.misses[i].better.name === m.better.name && c.misses[i].started.name === m.started.name && c.misses[i].slot === m.slot && same(c.misses[i].better.pts, m.better.pts) && same(c.misses[i].started.pts, m.started.pts) && same(c.misses[i].swing, m.swing)) ? [] : ['misses']),
    ...(c.waiver_misses.length === f.waiverMisses.length && f.waiverMisses.every((m, i) => c.waiver_misses[i].name === m.player.name && same(c.waiver_misses[i].pts, m.player.pts) && c.waiver_misses[i].over_name === m.over.name && same(c.waiver_misses[i].over_pts, m.over.pts)) ? [] : ['waiver_misses']),
  ];
  return share(7 - wrong.length, 7, `${wrong.join(', ')} differ from the tools`);
}

type Graded = RetroCard['misses'][number] | RetroCard['waiver_misses'][number];
const graded = (c: RetroCard): Graded[] => [...c.misses, ...c.waiver_misses];

function knowableDated(artifact: string): CodeResult {
  const rows = graded(parse<RetroCard>(artifact)).filter((r) => r.knowable);
  const bad = rows.filter((r) => !r.signal || !(Date.parse(r.signal.date) < Date.parse(r.before)));
  return share(rows.length - bad.length, rows.length, `${bad.length} knowable row(s) cite no signal dated before kickoff or the waiver run`);
}

/** Row units hold the graded miss and its news; `known` holds the rows that claim a pre-kickoff signal. */
export function retroEvidence(f: RetroFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse<RetroCard>(sealed.content);
    const news = [...f.misses, ...f.waiverMisses].map((r) => r.news);
    const unit = (r: Graded, i: number) => ({ id: `row${i + 1}`, state: { row: r, news: news[i] } as never });
    const rows = graded(card).map(unit);
    return {
      facts: [{ id: 'facts', state: f as never }],
      row: rows,
      known: graded(card).flatMap((r, i) => (r.knowable ? [rows[i]] : [])),
      card: [{ id: 'card', state: { summary: card.summary, lessons: card.lessons, won: card.won, left: card.left, swings: graded(card).map((r) => ('swing' in r ? r.swing : r.pts - r.over_pts)) } as never }],
    };
  };
}

// The audit's retro table (docs/v1/audits/sleeper.md), unreplayed: D45 asks for replay before any weight gates delivery.
export const retroRubric: Rubric = {
  name: 'sleeper.retro',
  version: 1,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [
      { id: 'tool_figures_copied', weight: 'gate', code: toolFiguresCopied },
      { id: 'knowable_dated_before_kickoff', weight: 'gate', code: knowableDated },
    ] },
    { id: 'grades_the_week', checks: [
      { id: 'signal_is_evidence', weight: 3, scope: 'known', question: 'Is the signal a dated news item or usage trend from the attached `news`, not the same injury designation restated?' },
      { id: 'lessons_actionable', weight: 2, scope: 'card', question: 'Are the lessons (at most three) things the manager can do next week?' },
      { id: 'summary_names_swing', weight: 2, scope: 'card', question: 'Does the summary name the single biggest swing among the `swings`?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'hindsight_called_process', weight: -4, scope: 'row', question: 'Is a row with knowable false (variance, no signal before kickoff) blamed on the manager\'s process?' },
    ] },
  ],
};
