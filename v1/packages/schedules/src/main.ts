import { checkedSettings, readDeploymentFile, serviceDbPath, serviceEntry, serviceToken } from '@quack/sdk';
import { settingsProblems, type SchedulesSettings } from './settings.ts';
import { createSchedulesService } from './service.ts';

function env(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const file = readDeploymentFile();
const entry = serviceEntry(file, 'schedules');
const settings = checkedSettings<SchedulesSettings>('schedules', entry.settings, settingsProblems);
const service = createSchedulesService({
  serviceToken: serviceToken(entry, 'schedules'),
  distributorUrl: env('QUACK_DISTRIBUTOR_URL'),
  dbPath: serviceDbPath(file, 'schedules'),
  agentsDir: process.env.QUACK_AGENTS_DIR,
  distributorToken: process.env.QUACK_ADMIN_TOKEN,
  retentionSweep: settings?.retentionSweep === true,
});
service.listen(Number(process.env.PORT ?? 8091));
service.start();
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => (service.stop(), process.exit(0)));
