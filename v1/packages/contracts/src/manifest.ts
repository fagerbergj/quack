import type { ExtensionManifest } from '@quack/sdk';
import { defineContract } from './action.ts';
import { contractsSessionExtensions, type ContractsConfig } from './index.ts';

export const manifest: ExtensionManifest = {
  name: 'contracts',
  optional: ['judge'],
  actions: [defineContract],
  session: (config) => contractsSessionExtensions(config as ContractsConfig),
};
