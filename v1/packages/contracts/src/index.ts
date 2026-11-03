import { defineExtension, type Extension } from '@earendil-works/pi-durable';
import { judge, type ClassifierRef, type ModelRef } from '@quack/judge';
import { artifactsExtension, type ArtifactKind } from '@quack/sdk';
import { contractRubric, specEvidence } from './rubric.ts';
import { CONTRACT_KIND, CONTRACT_PATH, summarizeSpec, validateSpec } from './spec.ts';
import { contractTools } from './tools.ts';

export { defineContract } from './action.ts';
export { askQuestionTool, YOU_DECIDE } from './ask.ts';
export { kinds, restKind, type ContractKind } from './kinds.ts';
export { breakingChanges, type Break } from './oasdiff.ts';
export { lintOpenApi, type Finding } from './rest-rules.ts';
export { contractRubric, guideAnchors, specEvidence } from './rubric.ts';
export { CONTRACT_KIND, CONTRACT_PATH, parseSpec, summarizeSpec, validateSpec, type Change, type Choice, type Compatibility, type ContractSpec, type Intent, type Why } from './spec.ts';
export { manifest } from './manifest.ts';

export const specKind: ArtifactKind = { kind: CONTRACT_KIND, path: CONTRACT_PATH, validate: validateSpec, summarize: summarizeSpec };

export type ContractsConfig = { maxRevisions?: number; classifier?: ClassifierRef; judgeModel?: ModelRef };

/** The contracts extension's session half: the question tool, the spec artifact and the judge that checks it against the kind's guide. */
export function contractsSessionExtensions(config: ContractsConfig = {}): Extension[] {
  return [
    defineExtension({ name: 'contracts', tools: contractTools }),
    artifactsExtension([specKind]),
    judge({ kind: specKind, rubric: contractRubric, evidence: specEvidence(), maxRevisions: config.maxRevisions ?? 2, classifier: config.classifier, judgeModel: config.judgeModel, toolName: 'submit_contract' }),
  ];
}
