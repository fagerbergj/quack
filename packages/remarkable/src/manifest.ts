import type { ExtensionManifest } from '@quack/sdk';
import { createRmClient } from './client.ts';
import { INGEST_WORKFLOW, ingestFromEnv } from './ingest/index.ts';
import { createRemarkableService } from './service.ts';

function required(env: NodeJS.ProcessEnv, name: string): string {
  const value = env[name];
  if (!value) throw new Error(`${name} is required by the remarkable app`);
  return value;
}

export const manifest: ExtensionManifest = {
  name: 'remarkable',
  requires: ['workflows'],
  optional: ['judge'],
  workflows: { [INGEST_WORKFLOW]: { build: (config) => ingestFromEnv(config as Parameters<typeof ingestFromEnv>[0]), env: [] } },
  // The rmfakecloud login is read from the environment by name, never the deployment file.
  pages: [{ method: 'GET', path: '/' }, { method: 'POST', path: '/ingest' }],
  app: ({ dbPath, distributorUrl, fetch, env }) => createRemarkableService({
    dbPath,
    distributorUrl,
    fetch,
    agent: 'remarkable-ingest',
    client: createRmClient({ url: required(env, 'RMFAKECLOUD_URL'), email: required(env, 'RMFAKECLOUD_EMAIL'), password: required(env, 'RMFAKECLOUD_PASSWORD') }),
  }),
};
