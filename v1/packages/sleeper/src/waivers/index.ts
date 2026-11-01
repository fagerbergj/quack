import { depsFromEnv } from '../deps.ts';
import { waiversWorkflow, WAIVERS_WORKFLOW } from './workflow.ts';

export { buildWaiversCard, type WaiversCard } from './card.ts';
export { buildWaiverFacts, type WaiverFacts } from './facts.ts';
export { nextProcessing, registerWaiversSchedules, startWaivers, waiversKey, waiversRun, WAIVERS_AGENT } from './jobs.ts';
export { waiversEvidence, waiversRubric } from './rubric.ts';
export { waiversWorkflow, WAIVERS_WORKFLOW, type WaiversDeps } from './workflow.ts';

/** The waivers workflow wired to the Sleeper API and the service half named in the environment. */
export const waiversFromEnv = (env: Record<string, string | undefined> = process.env) => waiversWorkflow(depsFromEnv(WAIVERS_WORKFLOW, env));
