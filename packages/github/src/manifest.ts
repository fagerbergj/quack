import { appEndpoint, type ExtensionManifest } from '@quack/sdk';
import { reviewPullRequest } from './action.ts';
import { evalAdapter } from './eval.ts';
import { mountGithub } from './mount.ts';
import { MIRROR_PATH } from './pulls.ts';
import { settingsProblems } from './settings.ts';
import { githubSessionExtensions, type GithubSessionConfig } from './post.ts';

// The launch credential comes from the environment, never the definition file.
export const manifest: ExtensionManifest = {
  name: 'github',
  optional: ['judge'],
  settings: settingsProblems,
  app: mountGithub,
  // The pull requests the page's badges read and their live events, and the webhook GitHub signs (the app verifies the signature).
  pages: [{ method: 'GET', path: MIRROR_PATH }, { method: 'GET', path: '/events' }, { method: 'POST', path: '/webhook' }],
  env: ['QUACK_INDEX_URL', 'QUACK_INDEX_TOKEN'],
  evalAdapters: [evalAdapter],
  actions: [reviewPullRequest],
  session: (config) => {
    const { url, token } = appEndpoint('github');
    return githubSessionExtensions({ ...(config as Partial<GithubSessionConfig>), serviceUrl: url, serviceToken: token });
  },
};
