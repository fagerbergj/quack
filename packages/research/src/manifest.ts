import type { ExtensionManifest } from '@quack/sdk';
import { researchQuestion } from './action.ts';
import { researchSessionExtensions } from './index.ts';
import { recordedReportEvidence, reportKind, researchRubric } from './report.ts';
import type { ResearchConfig } from './tools.ts';

export const manifest: ExtensionManifest = {
  name: 'research',
  optional: ['judge'],
  actions: [researchQuestion],
  env: ['QUACK_EXA_API_KEY'],
  evalAdapters: [{ kind: reportKind.kind, judge: { rubric: researchRubric, evidence: recordedReportEvidence } }],
  session: (config) => researchSessionExtensions(config as ResearchConfig),
};
