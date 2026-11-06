import type { Context } from '@earendil-works/chord/context';
import { defineExtension, type Extension, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { judge, type ClassifierRef, type ModelRef } from '@quack/judge';
import { artifactsExtension, type ArtifactKind, type SealedArtifact } from '@quack/sdk';
import { awaitingApproval, proposeVersion } from './approval.ts';
import { contractRubric, specEvidence } from './rubric.ts';
import { CONTRACT_KIND, CONTRACT_PATH, parseSpec, summarizeSpec, validateSpec } from './spec.ts';
import { contractTools } from './tools.ts';

export { APPROVED_KIND, CHANGES_KIND, proposeContractTool, routesOf } from './approval.ts';
export { defineContract } from './action.ts';
export { askQuestionTool, YOU_DECIDE } from './ask.ts';
export { jsonSchemaKind, kinds, restKind, typescriptKind, type ContractKind } from './kinds.ts';
export { breakingChanges, type Break } from './oasdiff.ts';
export { lintOpenApi, type Finding } from './rest-rules.ts';
export { lintJsonSchema } from './json-schema-rules.ts';
export { schemaBreaks } from './json-schema-diff.ts';
export { lintTypeScript } from './typescript-rules.ts';
export { typeScriptBreaks } from './typescript-diff.ts';
export { contractRubric, guideAnchors, specEvidence } from './rubric.ts';
export { CONTRACT_KIND, CONTRACT_PATH, parseSpec, summarizeSpec, validateSpec, type Change, type Choice, type Compatibility, type ContractSpec, type Intent, type Why } from './spec.ts';
export { manifest } from './manifest.ts';

// The version is data on the artifact: the summary is cut at 80 characters, which a long title would take it with.
const specOutcome = (content: string) => { const s = parseSpec(content); return typeof s === 'string' ? undefined : s.kind === 'rest' ? `v${s.version}` : `v${s.version} ${s.kind}`; };

export const specKind: ArtifactKind = { kind: CONTRACT_KIND, path: CONTRACT_PATH, validate: validateSpec, summarize: summarizeSpec, outcome: specOutcome };

// A pass puts that version to the person at once, so no separate proposal round is needed; one released unpassed at the limit is never put up.
const onCleared = async (sealed: SealedArtifact, v: { passed: boolean }, api: ToolExecutionApi, ctx: Context) =>
  v.passed ? proposeVersion(sealed, api, ctx) : { text: 'This version did not pass its checks, so it cannot be put to the person for approval. Tell them what failed and ask how to go on.' };

export type ContractsConfig ={ maxRevisions?: number; classifier?: ClassifierRef; judgeModel?: ModelRef };

/** The contracts extension's session half: the question tool, the spec artifact and the judge that checks it against the kind's guide. */
export function contractsSessionExtensions(config: ContractsConfig = {}): Extension[] {
  return [
    defineExtension({ name: 'contracts', tools: contractTools, hooks: [awaitingApproval()] }),
    artifactsExtension([specKind]),
    judge({ kind: specKind, rubric: contractRubric, evidence: specEvidence(), maxRevisions: config.maxRevisions ?? 2, classifier: config.classifier, judgeModel: config.judgeModel, toolName: 'submit_contract', onCleared }),
  ];
}
