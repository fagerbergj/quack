import { checkedSettings, readDeploymentFile, serviceDbPath, serviceEntry, serviceToken } from '@quack/sdk';
import { createSleeperClient } from './client.ts';
import { registerLineupSchedules } from './lineup/jobs.ts';
import { registerReportSchedules } from './report/jobs.ts';
import { removeLegacyUntilDone } from './schedules.ts';
import { registerFinderSchedules } from './trades/jobs.ts';
import { registerWaiversSchedules } from './waivers/jobs.ts';
import { registerTrendsSchedules } from './trends/jobs.ts';
import { createSleeperService } from './service.ts';
import { settingsProblems, type SleeperSettings } from './settings.ts';

const client = createSleeperClient();
const file = readDeploymentFile();
const entry = serviceEntry(file, 'sleeper');
const { owner, league } = checkedSettings<SleeperSettings>('sleeper', entry.settings, settingsProblems);
const scheduler = serviceEntry(file, 'schedules');
const schedules = { url: scheduler.url, token: serviceToken(scheduler, 'schedules') };
const service = createSleeperService({ serviceToken: serviceToken(entry, 'sleeper'), dbPath: serviceDbPath(file, 'sleeper'), client, distributorUrl: process.env.QUACK_DISTRIBUTOR_URL, owner, league, schedules });
service.listen(Number(process.env.PORT ?? 8092));
void removeLegacyUntilDone(schedules, league);
// A week's lineup runs depend on its kickoffs, which Sleeper publishes near the week, so re-register every few hours (idempotent).
if (owner) {
  const register = (fn: typeof registerLineupSchedules) => () => fn(schedules, client, league, owner).catch((err) => console.error(`${err.message}; will retry`));
  for (const job of [register(registerLineupSchedules), register(registerWaiversSchedules), register(registerFinderSchedules), register(registerTrendsSchedules), register(registerReportSchedules)]) {
    void job();
    setInterval(job, 6 * 60 * 60_000).unref();
  }
}
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => (service.stop(), process.exit(0)));
