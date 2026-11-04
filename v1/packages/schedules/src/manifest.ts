import type { ExtensionManifest } from '@quack/sdk';
import { settingsProblems } from './settings.ts';

/** The schedules service contributes no session extension; the manifest publishes its settings check. */
export const manifest: ExtensionManifest = { name: 'schedules', settings: settingsProblems };
