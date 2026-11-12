import { depsFromEnv } from '../deps.ts';
import { waiversWorkflow, WAIVERS_WORKFLOW, type WaiversDeps } from './workflow.ts';

export { buildWaiversCard, type WaiversCard } from './card.ts';
export { buildWaiverFacts, type WaiverFacts } from './facts.ts';
export { nextProcessing, registerWaiversSchedules, startWaivers, waiversKey, waiversRun, WAIVERS_AGENT } from './jobs.ts';
export { waiversDeclaration, waiversInputs, waiversInputsOf, waiversRubric } from './rubric.ts';
export { waiversWorkflow, WaiversFactsDoc, WAIVERS_FACTS_KIND, WAIVERS_WORKFLOW, type WaiversDeps } from './workflow.ts';

/** The waivers workflow wired to the Sleeper API and the service half named in the environment, judged with the agent's decision model. */
export const waiversFromEnv = (env: Record<string, string | undefined> = process.env, config?: { decisionModel?: WaiversDeps['decisionModel'] }) =>
  waiversWorkflow({ ...depsFromEnv(env), decisionModel: config?.decisionModel });
