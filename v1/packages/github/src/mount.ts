import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { checkedSettings, type App, type AppContext } from '@quack/sdk';
import { githubApp } from './app.ts';
import { startRedelivery } from './redeliver.ts';
import { assertPromoted, createGithubService } from './service.ts';
import { settingsProblems, type GithubSettings } from './settings.ts';

function required(env: NodeJS.ProcessEnv, name: string): string {
  const value = env[name];
  if (!value) throw new Error(`${name} is required by the github app`);
  return value;
}

/** The GitHub app; its webhook is /apps/github/webhook, which GitHub signs instead of presenting a credential. */
export function mountGithub({ settings, distributorUrl, fetch, env }: AppContext): App {
  const { implementerAgent, issueBase } = checkedSettings<GithubSettings>('github', settings, settingsProblems) ?? {};
  const agent = env.QUACK_GH_AGENT || 'reviewer';
  const dataDir = env.QUACK_DATA_DIR ?? '.';
  const app = githubApp({ appId: required(env, 'QUACK_GH_APP_ID'), privateKey: readFileSync(required(env, 'QUACK_GH_PRIVATE_KEY_FILE'), 'utf8') });
  const service = createGithubService(app, {
    webhookSecret: required(env, 'QUACK_GH_WEBHOOK_SECRET'),
    distributorUrl,
    fetch,
    label: env.QUACK_GH_LABEL || 'quack:review',
    agent,
    implementerAgent,
    issueBase,
    statesFile: join(dataDir, 'github-pulls.json'),
    queueFile: join(dataDir, 'github-fixes.json'),
  });
  let stopRedelivery = () => {};
  return {
    handle: service.handle,
    // Needs the distributor listening, so it runs from start; a draft agent must not back unattended triggers.
    async start() {
      await assertPromoted(distributorUrl, agent, undefined, fetch);
      if (implementerAgent) await assertPromoted(distributorUrl, implementerAgent, undefined, fetch);
      stopRedelivery = startRedelivery(app, join(dataDir, 'github-redeliver.json'));
      service.resume();
    },
    stop: () => { stopRedelivery(); service.stop(); },
  };
}
