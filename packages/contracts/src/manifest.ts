import type { ExtensionManifest } from '@quack/sdk';
import { defineContract } from './action.ts';
import { contractsApp, PAGE } from './app.ts';
import { contractsSessionExtensions } from './index.ts';
import { contractDeclaration, contractRubric, recordedSpecInputs } from './rubric.ts';
import { CONTRACT_KIND } from './spec.ts';

export const manifest: ExtensionManifest = {
  name: 'contracts',
  // The judge installs first, so its gate runs before the proposal: only a passed spec is put to the person.
  optional: ['judge'],
  actions: [defineContract],
  evalAdapters: [{ kind: CONTRACT_KIND, judge: { rubric: contractRubric, declaration: contractDeclaration, inputs: recordedSpecInputs } }],
  session: () => contractsSessionExtensions(),
  // The page reads it through the proxy like any page route; it reads only the catalog.
  app: (ctx) => contractsApp(ctx),
  pages: [{ method: 'GET', path: PAGE }],
};
