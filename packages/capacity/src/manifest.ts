import type { ExtensionManifest } from '@quack/sdk';
import { capacityLimits } from './limits.ts';
import { createCapacityService } from './service.ts';
import { capacityFromEnv } from './session.ts';

export const manifest: ExtensionManifest = {
  name: 'capacity',
  deployment: true,
  session: () => capacityFromEnv(),
  app: ({ dbPath, deployment }) => createCapacityService({ dbPath, capacity: capacityLimits({ providers: deployment.providers ?? {} }) }),
};
