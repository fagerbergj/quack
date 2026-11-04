import { appEndpoint, type ExtensionManifest } from '@quack/sdk';
import { reviewPullRequest } from './action.ts';
import { evalAdapter } from './eval.ts';
import { mountGithub } from './mount.ts';
import { settingsProblems } from './settings.ts';
import { githubSessionExtensions, type GithubSessionConfig } from './post.ts';

// The launch credential comes from the environment, never the definition file (D56).
export const manifest: ExtensionManifest = {
  name: 'github',
  optional: ['judge'],
  settings: settingsProblems,
  app: mountGithub,
  env: ['QUACK_INDEX_URL', 'QUACK_INDEX_TOKEN'],
  evalAdapters: [evalAdapter],
  actions: [reviewPullRequest],
  session: (config) => {
    const { url, token } = appEndpoint('github');
    return githubSessionExtensions({ ...(config as Partial<GithubSessionConfig>), serviceUrl: url, serviceToken: token });
  },
};
