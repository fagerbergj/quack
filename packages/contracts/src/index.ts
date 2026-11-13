import type { Context } from '@earendil-works/chord/context';
import { defineExtension, type Extension, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { judged } from '@quack/judge';
import { artifactsExtension, gate, sealedVersion, type ArtifactKind, type GateVerdict, type SealedArtifact } from '@quack/sdk';
import { awaitingApproval, proposeVersion } from './approval.ts';
import { contractDeclaration, contractRubric, specInputs } from './rubric.ts';
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
export { contractDeclaration, contractRubric, guideAnchors, guideRules, recordedSpecInputs, specInputs } from './rubric.ts';
export { CONTRACT_KIND, CONTRACT_PATH, parseSpec, summarizeSpec, validateSpec, type Change, type Choice, type Compatibility, type ContractSpec, type Intent, type Why } from './spec.ts';
export { manifest } from './manifest.ts';

// The version is data on the artifact: the summary is cut at 80 characters, which a long title would take it with.
const specOutcome = (content: string) => { const s = parseSpec(content); return typeof s === 'string' ? undefined : s.kind === 'rest' ? `v${s.version}` : `v${s.version} ${s.kind}`; };

export const specKind: ArtifactKind = { kind: CONTRACT_KIND, path: CONTRACT_PATH, tool: 'submit_contract', validate: validateSpec, summarize: summarizeSpec, outcome: specOutcome };

// A pass puts that version to the person at once and waits for them, so no separate proposal round is needed; one released unpassed at the limit is never put up.
// The gate runs after the judge's and reads the version back with its verdict; an unjudged version is not proposed.
async function proposeGate(event: { data: SealedArtifact }, p: { api?: ToolExecutionApi; ctx?: Context }): Promise<GateVerdict | undefined> {
  if (event.data.kind !== CONTRACT_KIND) return undefined;
  const sealed = (await sealedVersion(p.api!, CONTRACT_KIND, event.data.version, p.ctx!))!;
  const v = sealed.verdict as { passed: boolean } | null;
  if (!v) return undefined;
  const out = v.passed ? await proposeVersion(sealed, p.api!, p.ctx!) : { text: 'This version did not pass its checks, so it cannot be put to the person for approval. Tell them what failed and ask how to go on.' };
  return 'isError' in out && out.isError ? { kind: 'continue', feedback: out.text } : { kind: 'pass', note: out.text };
}

let unregister: (() => void) | undefined;

/** The contracts extension's session half: the question tool, and the spec artifact, judged against the kind's guide when the agent enables the judge and proposed once it passes; `inputs` is a seam for tests to watch what the judge reads. */
export function contractsSessionExtensions(inputs = specInputs()): Extension[] {
  judged(specKind.kind, { rubric: contractRubric, declaration: contractDeclaration, inputs });
  unregister?.();
  // The person's wait is exempt from the gate's bound: the approval waits through whileWaiting.
  unregister = gate('artifact.sealed', proposeGate);
  return [defineExtension({ name: 'contracts', tools: contractTools, hooks: [awaitingApproval()] }), artifactsExtension([specKind])];
}
export { contractOf as contractRecordOf, contractsApp, type ContractRecord } from './app.ts';
