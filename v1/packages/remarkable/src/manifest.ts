import type { ExtensionManifest } from '@quack/sdk';
import { INGEST_WORKFLOW, ingestFromEnv } from './ingest/index.ts';

export const manifest: ExtensionManifest = {
  name: 'remarkable',
  requires: ['workflows'],
  optional: ['judge'],
  workflows: { [INGEST_WORKFLOW]: { build: (config) => ingestFromEnv(config as Parameters<typeof ingestFromEnv>[0]), env: ['QUACK_REMARKABLE_URL', 'QUACK_REMARKABLE_TOKEN'] } },
};
