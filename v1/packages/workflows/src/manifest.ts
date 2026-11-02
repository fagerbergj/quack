import type { ExtensionManifest } from '@quack/sdk';
import { workflowSessionExtensions, type Workflow } from './index.ts';

const bound = (config: unknown) => (config as { workflow?: string })?.workflow ?? '';

// An agent selecting this binds its submissions to a workflow another extension contributed (WFL-1).
export const manifest: ExtensionManifest = {
  name: 'workflows',
  optional: ['judge'],
  env: (config, contributed) => contributed[bound(config)]?.env ?? [],
  session: (config, contributed) => {
    const name = bound(config);
    return workflowSessionExtensions(config, name in contributed ? { [name]: contributed[name].build(config) as Workflow } : undefined);
  },
};
