import type { JsonValue } from '@earendil-works/chord';
import { Type, type TSchema } from '@earendil-works/pi-ai';
import { rejudge, type JudgeOptions, type Rubric, type Verdict } from '@quack/judge';
import { attachVerdict, seal, type SealedArtifact } from '@quack/sdk';
import { defineWorkflow, type StepContext } from '@quack/workflows';
import { cardsKind } from '../lineup/card.ts';
import type { FactsDeps, LineupInput, NewsItem } from '../lineup/facts.ts';
import type { LineupDeps } from '../lineup/workflow.ts';
import { buildDigestCard, buildDigestFacts, type DigestDecision, type DigestFacts } from './digest.ts';
import { buildRetroCard, buildRetroFacts, type RetroDecision, type RetroFacts } from './retro.ts';
import { digestEvidence, digestRubric, retroEvidence, retroRubric } from './rubric.ts';

export const DIGEST_WORKFLOW = 'sleeper.digest';
export const RETRO_WORKFLOW = 'sleeper.retro';

/** The same wiring as the lineup job: the Sleeper API, today's snapshot and an optional news source. */
export type ReportDeps = LineupDeps;

type Spec<F, D> = {
  name: string;
  rubric: Rubric;
  /** Checks that need a news source; they are scored only once one is wired, and the verdict says so. */
  newsChecks: string[];
  facts: (deps: FactsDeps, snap: any, input: LineupInput, at: number) => Promise<F>;
  schema: TSchema;
  prompt: (f: F) => string;
  card: (f: F, d: D) => object;
  evidence: (f: F) => JudgeOptions['evidence'];
};

/** facts (code), one model decision, a sealed card (code), then the judge's verdict on it: the lineup job's shape. */
function reportWorkflow<F, D>(deps: ReportDeps, spec: Spec<F, D>) {
  const input = (c: StepContext) => JSON.parse(String(c.input)) as LineupInput;
  const rubric: Rubric = deps.news ? spec.rubric : { ...spec.rubric, criteria: spec.rubric.criteria.map((c) => ({ ...c, checks: c.checks.filter((k) => !spec.newsChecks.includes(k.id)) })) };
  const notes = deps.news ? [] : [`${spec.newsChecks.join(', ')} not checked: no news source`];
  return defineWorkflow({
    name: spec.name,
    steps: [
      {
        name: 'facts',
        kind: 'code',
        run: async (c) => {
          const i = input(c);
          return (await spec.facts({ client: deps.client, news: deps.news }, await deps.snapshot(i.league), i, deps.now ? deps.now() : c.api.now())) as unknown as JsonValue;
        },
      },
      { name: 'decide', kind: 'model', schema: spec.schema, prompt: (c) => spec.prompt(c.prior.facts as unknown as F) },
      {
        name: 'card',
        kind: 'code',
        run: async (c) => {
          await c.api.env!.writeFile(cardsKind.path, JSON.stringify(spec.card(c.prior.facts as unknown as F, c.prior.decide as unknown as D), null, 1), c.ctx);
          return (await seal(c.api, cardsKind, c.ctx)) as unknown as JsonValue;
        },
      },
      {
        name: 'check',
        kind: 'code',
        run: async (c) => {
          const sealed = c.prior.card as unknown as SealedArtifact;
          const options: JudgeOptions = { kind: cardsKind, rubric, evidence: spec.evidence(c.prior.facts as unknown as F), maxRevisions: 1, classifier: deps.classifier };
          const verdict: Verdict = await rejudge(c.api, options, sealed, `judge:${sealed.kind}:v${sealed.version}`, c.ctx);
          await attachVerdict(c.api, sealed.kind, sealed.version, { ...verdict, notes } as never, c.ctx);
          return { passed: verdict.passed, score: verdict.score, gatesFailed: verdict.gatesFailed, notes };
        },
      },
    ],
  });
}

const links = (items: Array<NewsItem & { player: string }>) => items.map((n) => ` [${n.title}](${n.url}), ${n.date.slice(0, 16)} (${n.player})`).join('');
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

const digestSchema = Type.Object({
  summary: Type.String({ maxLength: 400 }),
  notes: Type.Array(Type.Object({ matchup: Type.Integer(), note: Type.String({ maxLength: 240 }) })),
});

const retroLine = (i: number, text: string, news: Array<NewsItem & { player: string }>, before: string) => `${i}. ${text}\n   signals must be dated before ${before.slice(0, 16)}Z:${news.length ? links(news) : ' none found'}`;

function retroPrompt(f: RetroFacts): string {
  const misses = f.misses.map((m, i) => retroLine(i, `${m.slot}: started ${m.started.name} ${m.started.pts}, ${m.better.name} (${m.better.pos}) scored ${m.better.pts}, swing ${m.swing}`, m.news, m.before));
  const waiver = f.waiverMisses.map((w, i) => retroLine(i, `${w.player.name} (${w.player.pos}) scored ${w.player.pts} on the wire, over ${w.over.name} ${w.over.pts} at ${w.over.slot}`, w.news, w.before));
  return [
    `Week ${f.week} retro for ${f.team}: started ${f.started}${f.opp === null ? '' : ` against ${f.opponent} ${f.opp}`}, ${f.won ? 'won' : 'lost'}. The best legal lineup scored ${f.best}, ${f.left} left on the bench.`,
    `Lineup misses:\n${misses.join('\n') || 'none'}`,
    `Waiver misses:\n${waiver.join('\n') || 'none'}`,
    'For each row give its index and a one-sentence `why`. Mark `knowable` true only when you cite one of the linked items above as `signalUrl` that pointed the other way before the cutoff; a designation restated, or no link, means knowable false, which is variance and never the manager\'s fault. Give at most three lessons, each something to do next week, and a summary naming the single biggest swing.',
  ].join('\n\n');
}

const call = Type.Object({ index: Type.Integer({ minimum: 0 }), why: Type.String({ maxLength: 300 }), knowable: Type.Boolean(), signalUrl: Type.Optional(Type.String()) });
const retroSchema = Type.Object({ misses: Type.Array(call), waiverMisses: Type.Array(call), lessons: Type.Array(Type.String({ maxLength: 160 }), { maxItems: 3 }), summary: Type.String({ maxLength: 400 }) });

/** Digest job (SL-1): a preview before the week's first kickoff or a recap once its last game is final; code writes every number. */
export const digestWorkflow = (deps: ReportDeps) =>
  reportWorkflow<DigestFacts, DigestDecision>(deps, { name: DIGEST_WORKFLOW, rubric: digestRubric, newsChecks: ['preview_news_decides'], facts: buildDigestFacts, schema: digestSchema, prompt: digestPrompt, card: buildDigestCard, evidence: digestEvidence });

/** Retro job (SL-1): the owner's week after its last game; code builds the best lineup and the miss rows, the model writes `why`, `lessons` and `summary`. */
export const retroWorkflow = (deps: ReportDeps) =>
  reportWorkflow<RetroFacts, RetroDecision>(deps, { name: RETRO_WORKFLOW, rubric: retroRubric, newsChecks: ['signal_is_evidence'], facts: (d, snap, i) => buildRetroFacts(d, snap, i), schema: retroSchema, prompt: retroPrompt, card: buildRetroCard, evidence: retroEvidence });
