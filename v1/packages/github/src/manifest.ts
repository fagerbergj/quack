import type { ExtensionManifest } from '@quack/sdk';
import { reviewPullRequest } from './action.ts';
import { evalAdapter } from './eval.ts';
import { settingsProblems } from './settings.ts';
import { githubSessionExtensions, type GithubSessionConfig } from './post.ts';

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required by the github extension`);
  return value;
}

// The service secret comes from the environment, never the definition file (D56).
export const manifest: ExtensionManifest = {
  name: 'github',
  optional: ['judge'],
  settings: settingsProblems,
  env: ['QUACK_GITHUB_URL', 'QUACK_GITHUB_TOKEN'],
  evalAdapters: [evalAdapter],
  actions: [reviewPullRequest],
  session: (config) => githubSessionExtensions({ ...(config as Partial<GithubSessionConfig>), serviceUrl: required('QUACK_GITHUB_URL'), serviceToken: required('QUACK_GITHUB_TOKEN') }),
};
