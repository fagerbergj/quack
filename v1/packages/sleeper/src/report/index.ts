import { depsFromEnv } from '../deps.ts';
import { digestWorkflow, DIGEST_WORKFLOW, retroWorkflow, RETRO_WORKFLOW } from './workflow.ts';

export { buildDigestCard, buildDigestFacts, type DigestCard, type DigestFacts } from './digest.ts';
export { buildRetroCard, buildRetroFacts, type RetroCard, type RetroFacts } from './retro.ts';
export { DIGEST_AGENT, digestKey, registerReportSchedules, reportRuns, RETRO_AGENT, retroKey, startDigest, startRetro } from './jobs.ts';
export { digestEvidence, digestRubric, retroEvidence, retroRubric } from './rubric.ts';
export { digestWorkflow, DIGEST_WORKFLOW, retroWorkflow, RETRO_WORKFLOW, type ReportDeps } from './workflow.ts';

/** The report workflows wired to the Sleeper API and the service half named in the environment. */
export const digestFromEnv = (env: Record<string, string | undefined> = process.env) => digestWorkflow(depsFromEnv(DIGEST_WORKFLOW, env));
export const retroFromEnv = (env: Record<string, string | undefined> = process.env) => retroWorkflow(depsFromEnv(RETRO_WORKFLOW, env));
