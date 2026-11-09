import type { ExtensionManifest } from '@quack/sdk';
import { defineContract } from './action.ts';
import { contractsApp, PAGE } from './app.ts';
import { contractsSessionExtensions, type ContractsConfig } from './index.ts';
import { contractDeclaration, contractRubric, recordedSpecInputs } from './rubric.ts';
import { CONTRACT_KIND } from './spec.ts';

export const manifest: ExtensionManifest = {
  name: 'contracts',
  optional: ['judge'],
  actions: [defineContract],
  evalAdapters: [{ kind: CONTRACT_KIND, judge: { rubric: contractRubric, declaration: contractDeclaration, inputs: recordedSpecInputs } }],
  session: (config) => contractsSessionExtensions(config as ContractsConfig),
  // The page reads it through the proxy like any page route; it reads only the catalog.
  app: (ctx) => contractsApp(ctx),
  pages: [{ method: 'GET', path: PAGE }],
};
