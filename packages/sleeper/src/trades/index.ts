import { depsFromEnv } from '../deps.ts';
import { finderWorkflow, FINDER_WORKFLOW } from './workflow.ts';

export { buildFinderCard, type FinderCard } from './card.ts';
export { buildTradeFacts, type TradeFacts } from './facts.ts';
export { finderKey, finderRun, FINDER_AGENT, registerFinderSchedules, startFinder } from './jobs.ts';
export { tradeEvidence, tradeRubric } from './rubric.ts';
export { finderWorkflow, FINDER_WORKFLOW, type TradeJobDeps } from './workflow.ts';

/** The trade finder wired to the Sleeper API and the service half named in the environment. */
export const finderFromEnv = (env: Record<string, string | undefined> = process.env) => finderWorkflow(depsFromEnv(env));
