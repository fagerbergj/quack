import type { JsonValue } from '@earendil-works/chord';
import type { Schema } from '@earendil-works/pi-schema';
import type { Declaration, DecisionModelRef, JudgeOptions, Rubric } from '@quack/judge';
import type { Step, StepContext } from '@quack/workflows';
import { cardsKind } from './lineup/card.ts';
import type { LineupInput } from './lineup/facts.ts';

type JudgeDeps = { now?: () => number; decisionModel?: DecisionModelRef };

/** How a card is judged from the facts it was built on: a declaration over the named inputs those facts give. */
export type CardJudging<F> = { declaration: Declaration; inputs: (f: F) => unknown };

/** A job's card judge: its rubric, and its inputs from the facts its card was built on. */
export function cardJudge<F>(deps: JudgeDeps, rubric: Rubric, how: CardJudging<F>) {
  return (facts: F): { options: JudgeOptions } => ({ options: { kind: cardsKind, rubric, declaration: how.declaration, inputs: how.inputs(facts) as JudgeOptions['inputs'] & {}, decisionModel: deps.decisionModel, maxRevisions: 1 } });
}

/** What a card job is: how it gathers facts, asks for a decision, builds the card and judges it. */
export type CardJob<F, D> = {
  facts: (c: StepContext, input: LineupInput, at: number) => Promise<F>;
  schema: Schema;
  prompt: (f: F) => string;
  card: (f: F, d: D) => object;
  rubric: Rubric;
} & CardJudging<F>;

/** The facts step (code) and the judged card step every card job ends with. */
export function cardSteps<F, D>(deps: JudgeDeps, job: CardJob<F, D>): Step[] {
  const judgeOf = cardJudge(deps, job.rubric, job);
  const facts = (c: StepContext) => c.prior.facts as unknown as F;
  return [
    { name: 'facts', kind: 'code', run: async (c) => (await job.facts(c, JSON.parse(String(c.input)) as LineupInput, deps.now ? deps.now() : c.api.now())) as unknown as JsonValue },
    { name: 'card', kind: 'judged', schema: job.schema, prompt: (c) => job.prompt(facts(c)), card: (c, d) => job.card(facts(c), d as unknown as D), judge: (c) => judgeOf(facts(c)) },
  ];
}
