import { depsFromEnv } from '../deps.ts';
import { lineupWorkflow, LINEUP_WORKFLOW, type LineupDeps } from './workflow.ts';

export { buildCard, cardsKind, type LineupCard } from './card.ts';
export { buildFacts, type Facts } from './facts.ts';
export { LINEUP_AGENT, lineupKey, lineupRuns, registerLineupSchedules, startLineup } from './jobs.ts';
export { exaNews, newsLink, NEWS_WINDOW_MS } from './news.ts';
export { lineupDeclaration, lineupInputs, lineupInputsOf, lineupRubric } from './rubric.ts';
export { lineupWorkflow, LINEUP_WORKFLOW, type LineupDeps } from './workflow.ts';

/** The lineup workflow wired to the Sleeper API and the service half named in the environment, judged with the agent's decision model. */
export const lineupFromEnv = (env: Record<string, string | undefined> = process.env, config?: { decisionModel?: LineupDeps['decisionModel'] }) =>
  lineupWorkflow({ ...depsFromEnv(env), decisionModel: config?.decisionModel });
