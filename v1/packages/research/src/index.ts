import { defineExtension, type Extension } from '@earendil-works/pi-durable';
import { judge } from '@quack/judge';
import { artifactsExtension } from '@quack/sdk';
import { reportEvidence, reportKind, researchRubric } from './report.ts';
import { webFetchTool, webSearchTool, type ResearchConfig } from './tools.ts';

export { citedIds, reportEvidence, reportKind, researchRubric, validateReport } from './report.ts';
export { isPublicAddress, safeGet } from './net.ts';
export { formatResults, htmlToText, SourcesDoc, type ResearchConfig, type SourceRecord } from './tools.ts';

/** The research extension's session half: source tools, the fetch log, the report artifact and its judge. */
export function researchSessionExtensions(config: ResearchConfig = {}, deps: { isPublic?: (ip: string) => boolean } = {}): Extension[] {
  return [
    defineExtension({ name: 'research', tools: [webSearchTool(config), webFetchTool(config, deps.isPublic)] }),
    artifactsExtension([reportKind]),
    judge({ kind: reportKind, rubric: researchRubric, evidence: reportEvidence(), maxRevisions: config.maxRevisions ?? 2, classifier: config.classifier, judgeModel: config.judgeModel, toolName: 'submit_report' }),
  ];
}
