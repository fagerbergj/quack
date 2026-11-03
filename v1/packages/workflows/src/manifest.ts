import type { DeploymentView, ExtensionManifest } from '@quack/sdk';
import { workflowSessionExtensions, type Workflow } from './index.ts';

const bound = (config: unknown) => (config as { workflow?: string })?.workflow ?? '';

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);

/** A configured vision model must be declared in the deployment with images: true. */
export function visionProblems(config: unknown, deployment: DeploymentView): string[] {
  const ref = isObject(config) && isObject(config.visionModel) ? config.visionModel : undefined;
  if (!ref) return [];
  const { provider, modelId } = ref as { provider?: string; modelId?: string };
  const limits = typeof provider === 'string' && typeof modelId === 'string' ? deployment.providers[provider]?.models[modelId] : undefined;
  return limits?.images ? [] : [`workflows.visionModel ${String(provider)}/${String(modelId)} must be configured in this deployment with images: true`];
}

// An agent selecting this binds its submissions to a workflow another extension contributed (WFL-1).
export const manifest: ExtensionManifest = {
  name: 'workflows',
  optional: ['judge'],
  validate: visionProblems,
  env: (config, contributed) => contributed[bound(config)]?.env ?? [],
  session: (config, contributed) => {
    const name = bound(config);
    return workflowSessionExtensions(config, name in contributed ? { [name]: contributed[name].build(config) as Workflow } : undefined);
  },
};
