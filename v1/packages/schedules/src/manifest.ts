import { checkedSettings, type ExtensionManifest } from '@quack/sdk';
import { createSchedulesService } from './service.ts';
import { settingsProblems, type SchedulesSettings } from './settings.ts';

/** The schedules app contributes no session extension; the manifest publishes its settings check and mounts the app. */
export const manifest: ExtensionManifest = {
  name: 'schedules',
  settings: settingsProblems,
  app: ({ settings, dbPath, distributorUrl, env }) => createSchedulesService({
    distributorUrl,
    dbPath,
    agentsDir: env.QUACK_AGENTS_DIR,
    distributorToken: env.QUACK_ADMIN_TOKEN,
    retentionSweep: checkedSettings<SchedulesSettings>('schedules', settings, settingsProblems)?.retentionSweep === true,
  }),
};
