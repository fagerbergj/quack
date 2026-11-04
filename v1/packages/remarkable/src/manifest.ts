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
  workflows: { [INGEST_WORKFLOW]: { build: (config) => ingestFromEnv(config as Parameters<typeof ingestFromEnv>[0]), env: ['QUACK_INDEX_URL', 'QUACK_INDEX_TOKEN'] } },
  // The rmfakecloud login is read from the environment by name, never the deployment file.
  app: ({ dbPath, distributorUrl, env }) => createRemarkableService({
    dbPath,
    distributorUrl,
    agent: env.QUACK_REMARKABLE_AGENT || 'remarkable-ingest',
    client: createRmClient({ url: required(env, 'RMFAKECLOUD_URL'), email: required(env, 'RMFAKECLOUD_EMAIL'), password: required(env, 'RMFAKECLOUD_PASSWORD') }),
  }),
};
