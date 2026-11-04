import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { checkedSettings, readDeploymentFile, serviceEntry, serviceToken } from '@quack/sdk';
import { githubApp } from './app.ts';
import { startRedelivery } from './redeliver.ts';
import { settingsProblems, type GithubSettings } from './settings.ts';
import { assertPromoted, createGithubService } from './service.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

await assertPromoted(env('QUACK_DISTRIBUTOR_URL'), env('QUACK_GH_AGENT', 'reviewer'));
const entry = serviceEntry(readDeploymentFile(), 'github');
const implementerAgent = checkedSettings<GithubSettings>('github', entry.settings, settingsProblems)?.implementerAgent;
if (implementerAgent) await assertPromoted(env('QUACK_DISTRIBUTOR_URL'), implementerAgent);
const app = githubApp({ appId: env('QUACK_GH_APP_ID'), privateKey: readFileSync(env('QUACK_GH_PRIVATE_KEY_FILE'), 'utf8') });
const service = createGithubService(app, {
  webhookSecret: env('QUACK_GH_WEBHOOK_SECRET'),
  serviceToken: serviceToken(entry, 'github'),
  distributorUrl: env('QUACK_DISTRIBUTOR_URL'),
  label: env('QUACK_GH_LABEL', 'quack:review'),
  agent: env('QUACK_GH_AGENT', 'reviewer'),
  implementerAgent,
  statesFile: join(process.env.QUACK_DATA_DIR ?? '.', 'github-pulls.json'),
});
service.listen(Number(env('PORT', '8090')));
const stopRedelivery = startRedelivery(app, join(process.env.QUACK_DATA_DIR ?? '.', 'github-redeliver.json'));
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => { stopRedelivery(); service.close(() => process.exit(0)); });
