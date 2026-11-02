import type { ExtensionManifest } from '@quack/sdk';
import { researchSessionExtensions } from './index.ts';
import type { ResearchConfig } from './tools.ts';

export const manifest: ExtensionManifest = {
  name: 'research',
  optional: ['judge'],
  env: ['QUACK_EXA_API_KEY'],
  session: (config) => researchSessionExtensions(config as ResearchConfig),
};
