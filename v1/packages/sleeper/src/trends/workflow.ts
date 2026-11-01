import type { JsonValue } from '@earendil-works/chord';
import { Type } from '@earendil-works/pi-ai';
import { rejudge, type JudgeOptions, type Verdict } from '@quack/judge';
import { attachVerdict, seal, type SealedArtifact } from '@quack/sdk';
import { defineWorkflow, type StepContext } from '@quack/workflows';
import type { Json } from '../client.ts';
import { cardsKind } from '../lineup/card.ts';
import type { LineupInput } from '../lineup/facts.ts';
import type { LineupDeps } from '../lineup/workflow.ts';
import { buildTrendsCard, KINDS, when, type TrendsDecision } from './card.ts';
import { buildTrendsFacts, type SnapshotRef, type TrendsFacts } from './facts.ts';
import { NEWS_CHECKS, TEXT_MAX, trendsEvidence, trendsRubric } from './rubric.ts';

export const TRENDS_WORKFLOW = 'sleeper.trends';

/** The lineup wiring plus the service's snapshot history, which the diff reads. */
export type TrendsDeps = LineupDeps & {
  history: (league: string) => Promise<SnapshotRef[]>;
  snapshotOn: (league: string, date: string) => Promise<Json>;
};

const decisionSchema = Type.Object({
  items: Type.Array(
    Type.Object({
      kind: Type.Union(KINDS.map((k) => Type.Literal(k))),
      playerId: Type.Optional(Type.String()),
      newsUrl: Type.Optional(Type.String()),
      source: Type.Optional(Type.String({ maxLength: 60 })),
      text: Type.String({ maxLength: TEXT_MAX }),
    }),
    { maxItems: 12 },
  ),
  source_note: Type.String({ maxLength: 300 }),
});

function factsPrompt(f: TrendsFacts): string {
  const who = (id: string) => `${id} ${f.players[id].name} (${f.players[id].pos ?? '-'}, ${f.players[id].team ?? '-'})`;
  const links = (id: string) => (f.news[id] ?? []).map((n) => `\n    - [${n.title}](${n.url}), ${n.date.slice(0, 16)}`).join('');
  const changes = f.changes.map((c) => `- ${who(c.playerId)}: ${c.field} ${c.from ?? 'none'} -> ${c.to ?? 'none'}${links(c.playerId)}`);
  const rising = f.trending.map((t) => `- ${who(t.playerId)}: ${t.adds} adds in 24 hours, held by no team here`);
  return [
    f.since ? `Sleeper changes between ${when(Date.parse(`${f.since}T12:00:00Z`))} and ${when(Date.parse(`${f.date}T12:00:00Z`))}, week ${f.week}:\n${changes.join('\n') || 'none'}` : 'This is the first snapshot, so there is nothing to compare with.',
    `Free agents rising on Sleeper:\n${rising.join('\n') || 'none'}`,
    'Write one item per change worth a reader\'s attention. Name the player by id. A news link goes in `newsUrl` with `source` naming the reporter and outlet; use only the links above. When a dated report conflicts with Sleeper\'s designation, the item says so and which one you followed. `source_note` states each conflict and the rule followed, or "No source conflicts."',
  ].join('\n\n');
}

/** Trends job (SL-1): facts, one model step for prose, a sealed card, then the judge's verdict on it. */
export function trendsWorkflow(deps: TrendsDeps) {
  const input = (c: StepContext) => JSON.parse(String(c.input)) as LineupInput;
  // Without a news source no item can carry a link or a conflict, so those checks are not scored; the verdict says so.
  const rubric = deps.news ? trendsRubric : { ...trendsRubric, criteria: trendsRubric.criteria.map((c) => ({ ...c, checks: c.checks.filter((k) => !NEWS_CHECKS.includes(k.id)) })) };
  const notes = deps.news ? [] : ['source conflicts and original sources not checked: no news source'];
  const judgeOptions = (f: TrendsFacts): JudgeOptions => ({ kind: cardsKind, rubric, evidence: trendsEvidence(f) as never, maxRevisions: 1, classifier: deps.classifier });
  return defineWorkflow({
    name: TRENDS_WORKFLOW,
    steps: [
      {
        name: 'facts',
        kind: 'code',
        run: async (c) => {
          const i = input(c);
          return (await buildTrendsFacts(deps, await deps.snapshot(i.league), i, deps.now ? deps.now() : c.api.now())) as unknown as JsonValue;
        },
      },
      { name: 'decide', kind: 'model', schema: decisionSchema, prompt: (c) => factsPrompt(c.prior.facts as unknown as TrendsFacts) },
      {
        name: 'card',
        kind: 'code',
        run: async (c) => {
          const card = buildTrendsCard(c.prior.facts as unknown as TrendsFacts, c.prior.decide as unknown as TrendsDecision);
          await c.api.env!.writeFile(cardsKind.path, JSON.stringify(card, null, 1), c.ctx);
          return (await seal(c.api, cardsKind, c.ctx)) as unknown as JsonValue;
        },
      },
      {
        name: 'check',
        kind: 'code',
        run: async (c) => {
          const sealed = c.prior.card as unknown as SealedArtifact;
          const verdict: Verdict = await rejudge(c.api, judgeOptions(c.prior.facts as unknown as TrendsFacts), sealed, `judge:${sealed.kind}:v${sealed.version}`, c.ctx);
          await attachVerdict(c.api, sealed.kind, sealed.version, { ...verdict, notes } as never, c.ctx);
          return { passed: verdict.passed, score: verdict.score, gatesFailed: verdict.gatesFailed, notes };
        },
      },
    ],
  });
}
