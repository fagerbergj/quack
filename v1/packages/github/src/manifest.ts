import type { ExtensionManifest } from '@quack/sdk';
import { githubSessionExtensions, type GithubSessionConfig } from './post.ts';

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required by the github extension`);
  return value;
}

// The service secret comes from the environment, never the definition file (D56).
export const manifest: ExtensionManifest = {
  name: 'github',
  env: ['QUACK_GH_SERVICE_URL', 'QUACK_GH_SERVICE_TOKEN'],
  session: (config) => githubSessionExtensions({ ...(config as Partial<GithubSessionConfig>), serviceUrl: required('QUACK_GH_SERVICE_URL'), serviceToken: required('QUACK_GH_SERVICE_TOKEN') }),
};
