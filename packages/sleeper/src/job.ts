import type { JsonValue } from '@earendil-works/chord';
import type { Schema } from '@earendil-works/pi-schema';
import type { JudgeOptions, Rubric } from '@quack/judge';
import type { Step, StepContext } from '@quack/workflows';
import { cardsKind } from './lineup/card.ts';
import type { LineupInput } from './lineup/facts.ts';
import { newsGated, type NewsGate } from './rubric-kit.ts';

type JudgeDeps = { news?: unknown; now?: () => number };

/** A job's card judge: its rubric gated on the news source by newsGated, and evidence from the facts its card was built on. */
export function cardJudge<F>(deps: JudgeDeps, rubric: Rubric, gate: NewsGate | undefined, evidence: (f: F) => unknown) {
  const { rubric: gated, notes } = gate ? newsGated(rubric, gate, !!deps.news) : { rubric, notes: [] };
  return (facts: F): { options: JudgeOptions; notes: string[] } => ({ options: { kind: cardsKind, rubric: gated, evidence: evidence(facts) as NonNullable<JudgeOptions['evidence']>, maxRevisions: 1 }, notes });
}

/** What a card job is: how it gathers facts, asks for a decision, builds the card and judges it. */
export type CardJob<F, D> = {
  facts: (c: StepContext, input: LineupInput, at: number) => Promise<F>;
  schema: Schema;
  prompt: (f: F) => string;
  card: (f: F, d: D) => object;
  rubric: Rubric;
  news?: NewsGate;
  evidence: (f: F) => unknown;
};

/** The facts step (code) and the judged card step every card job ends with. */
export function cardSteps<F, D>(deps: JudgeDeps, job: CardJob<F, D>): Step[] {
  const judgeOf = cardJudge(deps, job.rubric, job.news, job.evidence);
  const facts = (c: StepContext) => c.prior.facts as unknown as F;
  return [
    { name: 'facts', kind: 'code', run: async (c) => (await job.facts(c, JSON.parse(String(c.input)) as LineupInput, deps.now ? deps.now() : c.api.now())) as unknown as JsonValue },
    { name: 'card', kind: 'judged', schema: job.schema, prompt: (c) => job.prompt(facts(c)), card: (c, d) => job.card(facts(c), d as unknown as D), judge: (c) => judgeOf(facts(c)) },
  ];
}
