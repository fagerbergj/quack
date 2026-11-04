import { checkedSettings, type App, type AppContext } from '@quack/sdk';
import { createSleeperClient } from './client.ts';
import { registerLineupSchedules } from './lineup/jobs.ts';
import { registerReportSchedules } from './report/jobs.ts';
import { removeLegacyUntilDone } from './schedules.ts';
import { createSleeperService } from './service.ts';
import { settingsProblems, type SleeperSettings } from './settings.ts';
import { registerFinderSchedules } from './trades/jobs.ts';
import { registerTrendsSchedules } from './trends/jobs.ts';
import { registerWaiversSchedules } from './waivers/jobs.ts';

const REREGISTER_MS = 6 * 60 * 60_000;

/** The Sleeper app: its routes, plus the schedule registration that keeps the league's jobs current. */
export function sleeperApp({ settings, dbPath, distributorUrl }: AppContext): App {
  const { owner, league } = checkedSettings<SleeperSettings>('sleeper', settings, settingsProblems);
  const client = createSleeperClient();
  const schedules = { url: `${distributorUrl}/apps/schedules` };
  const service = createSleeperService({ dbPath, client, distributorUrl, owner, league, schedules });
  const timers: NodeJS.Timeout[] = [];
  return {
    handle: service.handle,
    start() {
      void removeLegacyUntilDone(schedules, league);
      if (!owner) return;
      // A week's lineup runs depend on its kickoffs, which Sleeper publishes near the week, so re-register every few hours (idempotent).
      const register = (fn: typeof registerLineupSchedules) => () => fn(schedules, client, league, owner).catch((err) => console.error(`${err.message}; will retry`));
      for (const job of [register(registerLineupSchedules), register(registerWaiversSchedules), register(registerFinderSchedules), register(registerTrendsSchedules), register(registerReportSchedules)]) {
        void job();
        timers.push(setInterval(job, REREGISTER_MS).unref());
      }
    },
    stop() {
      timers.forEach(clearInterval);
      service.stop();
    },
  };
}
