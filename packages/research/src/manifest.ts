import type { ExtensionManifest } from '@quack/sdk';
import { researchQuestion } from './action.ts';
import { researchSessionExtensions } from './index.ts';
import { recordedReportInputs, reportDeclaration, reportKind, researchRubric } from './report.ts';
import type { ResearchConfig } from './tools.ts';

export const manifest: ExtensionManifest = {
  name: 'research',
  optional: ['judge'],
  actions: [researchQuestion],
  env: ['QUACK_EXA_API_KEY'],
  proxiedServices: [{ name: 'QUACK_EXA', urlEnv: 'QUACK_EXA_URL', defaultUrl: 'https://api.exa.ai', secretEnv: 'QUACK_EXA_API_KEY', header: 'x-api-key' }],
  evalAdapters: [{ kind: reportKind.kind, judge: { rubric: researchRubric, declaration: reportDeclaration, inputs: recordedReportInputs } }],
  session: (config) => researchSessionExtensions(config as ResearchConfig),
};
