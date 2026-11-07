import type { Schema } from '@earendil-works/pi-schema';
import { arr, bool, int, obj, opt, str } from '@earendil-works/pi-schema';
import { type JudgeOptions, type Rubric } from '@quack/judge';
import { defineWorkflow, Wait, type StepContext } from '@quack/workflows';
import { newsLink } from '../lineup/news.ts';
import { liveLeague } from '../snapshot.ts';
import { kickoffTimes } from '../games.ts';
import { cardSteps } from '../job.ts';
import type { NewsGate } from '../rubric-kit.ts';
import { round2, type FactsDeps, type LineupInput, type NewsItem } from '../lineup/facts.ts';
import type { LineupDeps } from '../lineup/workflow.ts';
import { RECAP_LAG_MS, RECAP_STALE_MS } from './jobs.ts';
import { buildDigestCard, buildDigestFacts, type DigestDecision, type DigestFacts } from './digest.ts';
import { buildRetroCard, buildRetroFacts, type RetroDecision, type RetroFacts } from './retro.ts';
import { DIGEST_NEWS, digestEvidence, digestRubric, RETRO_NEWS, retroEvidence, retroRubric } from './rubric.ts';
import { weekPhase, type Phase } from './week.ts';

export const DIGEST_WORKFLOW = 'sleeper.digest';
export const RETRO_WORKFLOW = 'sleeper.retro';
/** How often a run that fired before the games were final looks again. */
const RECHECK_MS = 60 * 60_000;

/** The same wiring as the lineup job: the Sleeper API and an optional news source. */
export type ReportDeps = LineupDeps;

type Spec<F, D> = {
  name: string;
  /** Whether the week is in a state this job can report on; otherwise the run sleeps and looks again. */
  ready: (phase: Phase) => boolean;
  rubric: Rubric;
  /** Checks that need a news source; they are scored only once one is wired, and the verdict says so. */
  news: NewsGate;
  facts: (deps: FactsDeps, snap: any, input: LineupInput, at: number) => Promise<F>;
  schema: Schema;
  prompt: (f: F) => string;
  card: (f: F, d: D) => object;
  evidence: (f: F) => JudgeOptions['evidence'];
};

/** facts (code), one model decision, a sealed card (code), then the judge's verdict on it: the lineup job's shape. */
function reportWorkflow<F, D>(deps: ReportDeps, spec: Spec<F, D>) {
  const input = (c: StepContext) => JSON.parse(String(c.input)) as LineupInput;
  return defineWorkflow({
    name: spec.name,
    steps: [
      {
        // A recap or retro fired early sleeps durably until the games are final, up to the run's own deadline, rather than failing and losing the week.
        name: 'ready',
        kind: 'code',
        run: async (c) => {
          const i = input(c);
          const { league, state } = (await liveLeague(deps.client, i.league)).data;
          const [schedule, scores] = await Promise.all([deps.client.schedule(league.season), deps.client.scores(league.season, i.week)]);
          const phase = weekPhase(schedule, i.week, state.week);
          if (spec.ready(phase)) return { phase };
          const now = deps.now ? deps.now() : c.api.now();
          const starts = kickoffTimes(scores);
          if (!starts.length) throw new Error(`sleeper: week ${i.week} has no kickoff, so the run cannot wait for it to be final`);
          const give = Math.max(...starts) + RECAP_LAG_MS + RECAP_STALE_MS;
          if (now >= give) throw new Error(`sleeper: week ${i.week} was still ${phase === 'live' ? 'in progress' : 'not final'} at this run's deadline`);
          return new Wait(Math.min(now + RECHECK_MS, give));
        },
      },
      ...cardSteps<F, D>(deps, { ...spec, facts: async (_c, i, at) => spec.facts({ client: deps.client, news: deps.news }, await liveLeague(deps.client, i.league), i, at) }),
    ],
  });
}

const links = (items: Array<NewsItem & { player: string }>) => items.map((n) => ` ${newsLink(n)} (${n.player})`).join('');
const scorers = (s: DigestFacts['games'][number]['sides'][number]) => s.top.map((t) => `${t.name} ${t.pts}`).join(', ') || 'none';

function digestPrompt(f: DigestFacts): string {
  const preview = f.mode === 'preview';
  const games = f.games.map((g) => {
    const [a, b] = g.sides;
    const news = g.sides.flatMap((s) => [...s.top, ...s.designated].flatMap((p) => f.news[p.id] ?? []));
    return [
      `[${g.id}] ${a.team} (${a.record}) ${a.pts} vs ${b.team} (${b.record}) ${b.pts}, margin ${g.margin}${g.mine ? ' (your game)' : ''}`,
      `  top: ${a.team}: ${scorers(a)}; ${b.team}: ${scorers(b)}`,
      ...(preview ? [`  designations: ${g.sides.flatMap((s) => s.designated.map((d) => `${d.name} ${d.inj}`)).join(', ') || 'none'}; counted 0: ${g.sides.flatMap((s) => s.out).join(', ') || 'nobody'}`] : []),
      ...(news.length ? [`  news:${links(news)}`] : []),
    ].join('\n');
  });
  const table = f.standings.map((s) => `${s.rank}. ${s.team} ${s.wins}-${s.losses}${s.ties ? `-${s.ties}` : ''}, ${s.fpts} PF${s.in_playoff_line ? ' [in the playoff line]' : ''}`);
  return [
    `Week ${f.week} ${preview ? 'preview' : 'recap'}. ${f.sourceNote}`,
    preview ? 'The points shown are each side\'s code-computed lineup average over recent finished weeks, not a forecast.' : 'The points shown are final.',
    `Games:\n${games.join('\n')}`,
    `Standings (the top ${f.playoffTeams} make the playoffs; ${f.remainingWeeks} weeks until they start):\n${table.join('\n')}`,
    ...(Object.values(f.news).some((n) => n.length) ? ['Linked news is dated. A newer credible report beats the Sleeper designation, which lags; say which you followed when they conflict.'] : []),
    `Write one note per game, keyed by its number. ${preview ? 'Cite the news or designation that could swing it, as a markdown link when one is given.' : 'Say what decided it; never describe game flow the score alone does not show.'} Never state a projection, a projected total, a win probability or playoff odds: the standings table is the only playoff line. Label claims from under three weeks of data. The summary is the week's one real story.`,
  ].join('\n\n');
}

const digestSchema = obj({
  summary: str({ maxLength: 400 }),
  notes: arr(obj({ matchup: int(), note: str({ maxLength: 240 }) }, { open: true })),
}, { open: true });

const retroLine = (i: number, text: string, news: Array<NewsItem & { player: string }>, before: string) => `${i}. ${text}\n   signals must be dated before ${before.slice(0, 16)}Z:${news.length ? links(news) : ' none found'}`;

/** The largest swing across both row lists, as the prompt names it; null with no rows. A tie goes to the lineup miss. */
function biggestSwing(f: RetroFacts): string | null {
  const rows = [
    ...f.misses.map((m, i) => ({ ref: `lineup miss ${i}`, who: `${m.better.name} over ${m.started.name}`, swing: m.swing })),
    ...f.waiverMisses.map((w, i) => ({ ref: `waiver miss ${i}`, who: `${w.player.name} over ${w.over.name}`, swing: round2(w.player.pts - w.over.pts) })),
  ];
  const top = rows.reduce<(typeof rows)[number] | null>((a, r) => (!a || r.swing > a.swing ? r : a), null);
  return top && `Biggest swing: ${top.ref}, ${top.who}, ${top.swing}. The summary names it.`;
}

export function retroPrompt(f: RetroFacts): string {
  const misses = f.misses.map((m, i) => retroLine(i, `${m.slot}: started ${m.started.name} ${m.started.pts}, ${m.better.name} (${m.better.pos}) scored ${m.better.pts}, swing ${m.swing}`, m.news, m.before));
  const waiver = f.waiverMisses.map((w, i) => retroLine(i, `${w.player.name} (${w.player.pos}) scored ${w.player.pts} on the wire, over ${w.over.name} ${w.over.pts} at ${w.over.slot}`, w.news, w.before));
  return [
    `Week ${f.week} retro for ${f.team}: ${f.bye ? 'was on bye' : `started ${f.started}${f.opp === null ? '' : ` against ${f.opponent} ${f.opp}`}, ${f.won ? 'won' : 'lost'}`}. The best legal lineup scored ${f.best}, ${f.left} left on the bench.`,
    `Lineup misses:\n${misses.join('\n') || 'none'}`,
    `Waiver misses:\n${waiver.join('\n') || 'none'}`,
    ...[biggestSwing(f)].filter((l) => l !== null),
    'For each row give its index and a one-sentence `why`. Mark `knowable` true only when you cite one of the linked items above as `signalUrl` that pointed the other way before the cutoff; a designation restated, or no link, means knowable false, which is variance and never the manager\'s fault. Give at most three lessons, each something to do next week, and a summary naming the single biggest swing.',
  ].join('\n\n');
}

const call = obj({ index: int({ minimum: 0 }), why: str({ maxLength: 300 }), knowable: bool(), signalUrl: opt(str()) }, { open: true });
const retroSchema = obj({ misses: arr(call), waiverMisses: arr(call), lessons: arr(str({ maxLength: 160 }), { maxItems: 3 }), summary: str({ maxLength: 400 }) }, { open: true });

/** Digest job: a preview before the week's first kickoff or a recap once its last game is final; code writes every number. */
export const digestWorkflow = (deps: ReportDeps) =>
  reportWorkflow<DigestFacts, DigestDecision>(deps, { name: DIGEST_WORKFLOW, ready: (p) => p !== 'live', rubric: digestRubric, news: DIGEST_NEWS, facts: buildDigestFacts, schema: digestSchema, prompt: digestPrompt, card: buildDigestCard, evidence: digestEvidence });

/** Retro job: the owner's week after its last game; code builds the best lineup and the miss rows, the model writes `why`, `lessons` and `summary`. */
export const retroWorkflow = (deps: ReportDeps) =>
  reportWorkflow<RetroFacts, RetroDecision>(deps, { name: RETRO_WORKFLOW, ready: (p) => p === 'recap', rubric: retroRubric, news: RETRO_NEWS, facts: (d, snap, i) => buildRetroFacts(d, snap, i), schema: retroSchema, prompt: retroPrompt, card: buildRetroCard, evidence: retroEvidence });
