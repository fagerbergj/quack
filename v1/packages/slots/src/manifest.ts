import type { ExtensionManifest } from '@quack/sdk';
import { slotsFromEnv } from './session.ts';

export const manifest: ExtensionManifest = { name: 'slots', deployment: true, env: ['QUACK_SLOTS_URL', 'QUACK_SLOTS_TOKEN'], session: () => slotsFromEnv() };
