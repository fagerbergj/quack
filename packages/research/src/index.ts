import { defineExtension, type Extension } from '@earendil-works/pi-durable';
import { judge } from '@quack/judge';
import { artifactsExtension } from '@quack/sdk';
import { reportDeclaration, reportInputs, reportKind, researchRubric } from './report.ts';
import { readSourceTool, webFetchTool, webSearchTool, type ResearchConfig } from './tools.ts';

export { deliveredReport } from './marks.ts';
export { citedIds, findingItems, inputsOf, recordedReportInputs, reportDeclaration, reportInputs, reportKind, researchRubric, validateReport } from './report.ts';
export { isPublicAddress, safeGet } from './net.ts';
export { formatResults, htmlToText, sourceText, SourcesDoc, type ResearchConfig, type SourceRecord } from './tools.ts';

/** The research extension's session half: source tools, the fetch log, the report artifact and its judge. */
export function researchSessionExtensions(config: ResearchConfig = {}, deps: { isPublic?: (ip: string) => boolean; inputs?: ReturnType<typeof reportInputs> } = {}): Extension[] {
  return [
    defineExtension({ name: 'research', tools: [webSearchTool(config), webFetchTool(config, deps.isPublic), readSourceTool()] }),
    artifactsExtension([reportKind]),
    judge({ kind: reportKind, rubric: researchRubric, declaration: reportDeclaration, inputs: deps.inputs ?? reportInputs(), maxRevisions: config.maxRevisions ?? 2, classifier: config.classifier, judgeModel: config.judgeModel, toolName: 'submit_report' }),
  ];
}
export { manifest } from './manifest.ts';
