import { defineExtension, type Extension } from '@earendil-works/pi-durable';
import { judged } from '@quack/judge';
import { artifactsExtension } from '@quack/sdk';
import { reportDeclaration, reportInputs, reportKind, researchRubric } from './report.ts';
import { readSourceTool, webFetchTool, webSearchTool, type ResearchConfig } from './tools.ts';

export { citedIds, findingItems, inputsOf, recordedReportInputs, reportDeclaration, reportInputs, reportKind, researchRubric, validateReport } from './report.ts';
export { isPublicAddress, safeGet } from './net.ts';
export { formatResults, htmlToText, sourceText, SourcesDoc, type ResearchConfig, type SourceRecord } from './tools.ts';

/** The research extension's session half: source tools, the fetch log, and the report artifact, judged when the agent enables the judge. */
export function researchSessionExtensions(config: ResearchConfig = {}, deps: { isPublic?: (ip: string) => boolean; inputs?: ReturnType<typeof reportInputs> } = {}): Extension[] {
  judged(reportKind.kind, { rubric: researchRubric, declaration: reportDeclaration, inputs: deps.inputs ?? reportInputs() });
  return [
    defineExtension({ name: 'research', tools: [webSearchTool(config), webFetchTool(config, deps.isPublic), readSourceTool()] }),
    artifactsExtension([reportKind]),
  ];
}
export { manifest } from './manifest.ts';
