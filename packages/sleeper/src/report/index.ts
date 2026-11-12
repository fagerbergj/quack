import { depsFromEnv } from '../deps.ts';
import { digestWorkflow, DIGEST_WORKFLOW, retroWorkflow, RETRO_WORKFLOW, type ReportDeps } from './workflow.ts';

export { buildDigestCard, buildDigestFacts, type DigestCard, type DigestFacts } from './digest.ts';
export { buildRetroCard, buildRetroFacts, type RetroCard, type RetroFacts } from './retro.ts';
export { DIGEST_AGENT, digestKey, registerReportSchedules, reportRuns, RETRO_AGENT, retroKey, startDigest, startRetro } from './jobs.ts';
export { digestDeclaration, digestInputs, digestInputsOf, digestRubric, retroDeclaration, retroInputs, retroInputsOf, retroRubric } from './rubric.ts';
export { digestWorkflow, DIGEST_WORKFLOW, retroWorkflow, RETRO_WORKFLOW, type ReportDeps } from './workflow.ts';

type Config = { decisionModel?: ReportDeps['decisionModel'] };
/** The report workflows wired to the Sleeper API and the service half named in the environment, judged with the agent's decision model. */
export const digestFromEnv = (env: Record<string, string | undefined> = process.env, config?: Config) => digestWorkflow({ ...depsFromEnv(env), decisionModel: config?.decisionModel });
export const retroFromEnv = (env: Record<string, string | undefined> = process.env, config?: Config) => retroWorkflow({ ...depsFromEnv(env), decisionModel: config?.decisionModel });
