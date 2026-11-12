import { depsFromEnv } from '../deps.ts';
import { finderWorkflow, FINDER_WORKFLOW, type TradeJobDeps } from './workflow.ts';

export { buildFinderCard, type FinderCard } from './card.ts';
export { buildTradeFacts, type TradeFacts } from './facts.ts';
export { finderKey, finderRun, FINDER_AGENT, registerFinderSchedules, startFinder } from './jobs.ts';
export { tradeDeclaration, tradeInputs, tradeInputsOf, tradeRubric } from './rubric.ts';
export { finderWorkflow, FINDER_WORKFLOW, type TradeJobDeps } from './workflow.ts';

/** The trade finder wired to the Sleeper API and the service half named in the environment, judged with the agent's decision model. */
export const finderFromEnv = (env: Record<string, string | undefined> = process.env, config?: { decisionModel?: TradeJobDeps['decisionModel'] }) =>
  finderWorkflow({ ...depsFromEnv(env), decisionModel: config?.decisionModel });
