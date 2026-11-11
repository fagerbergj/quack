import type { ExtensionManifest } from '@quack/sdk';
import { slotLimits } from './limits.ts';
import { createSlotsService } from './service.ts';
import { slotsFromEnv } from './session.ts';

export const manifest: ExtensionManifest = { name: 'slots', deployment: true, session: () => slotsFromEnv(), app: ({ dbPath, deployment }) => createSlotsService({ dbPath, limits: slotLimits({ providers: deployment.providers ?? {} }) }) };
