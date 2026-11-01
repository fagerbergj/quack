import type { JsonValue } from '@earendil-works/chord';
import { Type } from '@earendil-works/pi-ai';
import { rejudge, type ClassifierRef, type JudgeOptions, type Verdict } from '@quack/judge';
import { attachVerdict, seal, type SealedArtifact } from '@quack/sdk';
import { defineWorkflow, type StepContext } from '@quack/workflows';
import type { Json, SleeperClient } from '../client.ts';
import { buildCard, cardsKind, type Decision } from './card.ts';
import { buildFacts, type Facts, type FactsDeps, type LineupInput, type PlayerFact } from './facts.ts';
import { lineupEvidence, lineupRubric } from './rubric.ts';

export const LINEUP_WORKFLOW = 'sleeper.lineup';

export type LineupDeps = {
  client: SleeperClient;
  /** Today's league snapshot from the service half (SL-5). */
  snapshot: (league: string) => Promise<Json>;
  news?: FactsDeps['news'];
  classifier?: ClassifierRef;
  now?: () => number;
};

const decisionSchema = Type.Object({
  slots: Type.Array(Type.Object({ slot: Type.String(), playerId: Type.String(), why: Type.String({ maxLength: 240 }) })),
  summary: Type.String({ maxLength: 400 }),
});

const line = (p: PlayerFact) => {
  const g = p.game ? `${p.game.home ? 'vs' : 'at'} ${p.game.opponent}, kickoff ${p.game.kickoff ?? 'unknown'}` : 'no game';
  return `${p.id} ${p.name} (${p.pos}, ${p.team ?? '-'}) proj ${p.proj}${p.inj ? `, ${p.inj}` : ''}, ${g}`;
};

function factsPrompt(f: Facts): string {
  const now = (s: string) => (f.current[s] ? line(f.players[f.current[s]!]) : 'empty');
  const open = f.open.map((s) => `${s} (now ${now(s)})\n${f.candidates[s].map((id) => `  - ${line(f.players[id])}${(f.news[id] ?? []).map((n) => ` [${n.title}](${n.url})`).join('')}`).join('\n')}`);
  const locked = f.slots.filter((s) => !f.open.includes(s)).map((s) => `${s}: ${now(s)}`);
  const base = f.baseline.map((b) => `${b.slot}: ${b.name} ${b.proj}`);
  return [
    `Week ${f.week}, ${f.team} (${f.record}) against ${f.opponent ?? 'a bye'}.`,
    `Best lineup by projection (the baseline; Out, IR, Doubtful and Sus excluded):\n${base.join('\n')}`,
    `Locked slots, final this week:\n${locked.join('\n') || 'none'}`,
    `Open slots with the players who can fill them:\n${open.join('\n')}`,
    'Decide every open slot, FLEX included, by player id. Write the summary as "Start X over Y" for each change from the current lineup, or say there are no changes.',
  ].join('\n\n');
}

/** Lineup job (SL-1): facts, one model decision, a sealed card, then the judge's verdict on it. */
export function lineupWorkflow(deps: LineupDeps) {
  const input = (c: StepContext) => JSON.parse(String(c.input)) as LineupInput;
  // close_call_news cannot be met without a news source, so it is scored only once one is wired; the verdict says so.
  const rubric = deps.news ? lineupRubric : { ...lineupRubric, criteria: lineupRubric.criteria.map((c) => ({ ...c, checks: c.checks.filter((k) => k.id !== 'close_call_news') })) };
  const notes = deps.news ? [] : ['close-call news not checked: no news source'];
  const judgeOptions = (f: Facts): JudgeOptions => ({ kind: cardsKind, rubric, evidence: lineupEvidence(f) as never, maxRevisions: 1, classifier: deps.classifier });
  return defineWorkflow({
    name: LINEUP_WORKFLOW,
    steps: [
      {
        name: 'facts',
        kind: 'code',
        run: async (c) => {
          const i = input(c);
          return (await buildFacts({ client: deps.client, news: deps.news }, await deps.snapshot(i.league), i, (deps.now ? deps.now() : c.api.now()))) as unknown as JsonValue;
        },
      },
      { name: 'decide', kind: 'model', schema: decisionSchema, prompt: (c) => factsPrompt(c.prior.facts as unknown as Facts) },
      {
        name: 'card',
        kind: 'code',
        run: async (c) => {
          const card = buildCard(c.prior.facts as unknown as Facts, c.prior.decide as unknown as Decision);
          await c.api.env!.writeFile(cardsKind.path, JSON.stringify(card, null, 1), c.ctx);
          return (await seal(c.api, cardsKind, c.ctx)) as unknown as JsonValue;
        },
      },
      {
        name: 'check',
        kind: 'code',
        run: async (c) => {
          const sealed = c.prior.card as unknown as SealedArtifact;
          const verdict: Verdict = await rejudge(c.api, judgeOptions(c.prior.facts as unknown as Facts), sealed, `judge:${sealed.kind}:v${sealed.version}`, c.ctx);
          await attachVerdict(c.api, sealed.kind, sealed.version, { ...verdict, notes } as never, c.ctx);
          return { passed: verdict.passed, score: verdict.score, gatesFailed: verdict.gatesFailed, notes };
        },
      },
    ],
  });
}
