import { depsFromEnv } from '../deps.ts';
import { finderWorkflow, FINDER_WORKFLOW, tradeWorkflow, TRADE_WORKFLOW } from './workflow.ts';

export { buildFinderCard, buildTradeCard, type FinderCard, type TradeCard } from './card.ts';
export { buildTradeFacts, type TradeFacts } from './facts.ts';
export { finderKey, finderRun, FINDER_AGENT, registerFinderSchedules, startFinder, startTrade, TRADE_AGENT, tradeKey, tradeParams } from './jobs.ts';
export { tradeEvidence, tradeRubric } from './rubric.ts';
export { finderWorkflow, FINDER_WORKFLOW, tradeWorkflow, TRADE_WORKFLOW, type TradeJobDeps } from './workflow.ts';

/** The trade workflows wired to the Sleeper API and the service half named in the environment. */
export const finderFromEnv = (env: Record<string, string | undefined> = process.env) => finderWorkflow(depsFromEnv(FINDER_WORKFLOW, env));
export const tradeFromEnv = (env: Record<string, string | undefined> = process.env) => tradeWorkflow(depsFromEnv(TRADE_WORKFLOW, env));
