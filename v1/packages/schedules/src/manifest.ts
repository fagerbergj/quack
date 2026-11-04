import { checkedSettings, type ExtensionManifest } from '@quack/sdk';
import { createSchedulesService } from './service.ts';
import { settingsProblems, type SchedulesSettings } from './settings.ts';

/** The schedules app contributes no session extension; the manifest publishes its settings check and mounts the app. */
export const manifest: ExtensionManifest = {
  name: 'schedules',
  settings: settingsProblems,
  // The schedules page lists and deletes; registering is for sessions and the sleeper app.
  pages: [{ method: 'GET', path: '/' }, { method: 'DELETE', path: /^\/.+$/ }],
  app: ({ settings, dbPath, distributorUrl, fetch, env }) => createSchedulesService({
    distributorUrl,
    fetch,
    dbPath,
    agentsDir: env.QUACK_AGENTS_DIR,
    distributorToken: env.QUACK_ADMIN_TOKEN,
    retentionSweep: checkedSettings<SchedulesSettings>('schedules', settings, settingsProblems)?.retentionSweep === true,
  }),
};
