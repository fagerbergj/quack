import { readFileSync } from 'node:fs';
import { githubApp } from './app.ts';
import { assertPromoted, createGithubService } from './service.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

await assertPromoted(env('QUACK_DISTRIBUTOR_URL'), env('QUACK_GH_AGENT', 'reviewer'));
const app = githubApp({ appId: env('QUACK_GH_APP_ID'), privateKey: readFileSync(env('QUACK_GH_PRIVATE_KEY_FILE'), 'utf8') });
const service = createGithubService(app, {
  webhookSecret: env('QUACK_GH_WEBHOOK_SECRET'),
  serviceToken: env('QUACK_GH_SERVICE_TOKEN'),
  distributorUrl: env('QUACK_DISTRIBUTOR_URL'),
  label: env('QUACK_GH_LABEL', 'quack:review'),
  agent: env('QUACK_GH_AGENT', 'reviewer'),
});
service.listen(Number(env('PORT', '8090')));
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => service.close(() => process.exit(0)));
