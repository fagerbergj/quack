import type { ExtensionManifest } from '@quack/sdk';
import { researchQuestion } from './action.ts';
import { researchSessionExtensions } from './index.ts';
import { reportKind, researchRubric } from './report.ts';
import type { ResearchConfig } from './tools.ts';

export const manifest: ExtensionManifest = {
  name: 'research',
  optional: ['judge'],
  actions: [researchQuestion],
  env: ['QUACK_EXA_API_KEY'],
  // shortcut: no rebuilt evidence, since claim checks need source files a recording does not keep (judge replay takes inline evidence).
  evalAdapters: [{ kind: reportKind.kind, judge: { rubric: researchRubric, evidence: async () => undefined } }],
  session: (config) => researchSessionExtensions(config as ResearchConfig),
};
