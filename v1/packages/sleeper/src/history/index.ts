import { depsFromEnv } from '../deps.ts';
import { historyWorkflow, HISTORY_WORKFLOW } from './workflow.ts';

export { buildHistoryCard, type HistoryCard } from './card.ts';
export { buildHistoryFacts, DEFINITIONS, REACH_STEAL_GAP, verdictOf, type HistoryFacts } from './facts.ts';
export { HISTORY_AGENT, historyKey, historyParams, startHistory } from './jobs.ts';
export { historyEvidence, historyRubric } from './rubric.ts';
export { historyWorkflow, HISTORY_WORKFLOW, type HistoryDeps } from './workflow.ts';

/** The history workflow wired to the Sleeper API and the service half named in the environment. */
export const historyFromEnv = (env: Record<string, string | undefined> = process.env) => historyWorkflow(depsFromEnv(HISTORY_WORKFLOW, env));
