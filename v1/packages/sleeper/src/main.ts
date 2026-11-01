import { createSleeperClient } from './client.ts';
import { registerLineupSchedules } from './lineup/jobs.ts';
import { registerReportSchedules } from './report/jobs.ts';
import { registerUntilDone } from './schedules.ts';
import { registerFinderSchedules } from './trades/jobs.ts';
import { registerWaiversSchedules } from './waivers/jobs.ts';
import { registerTrendsSchedules } from './trends/jobs.ts';
import { createSleeperService } from './service.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const client = createSleeperClient();
const owner = process.env.QUACK_SLEEPER_OWNER;
const service = createSleeperService({ serviceToken: env('QUACK_SLEEPER_TOKEN'), dbPath: env('QUACK_SLEEPER_DB', '/data/sleeper.db'), client, distributorUrl: process.env.QUACK_DISTRIBUTOR_URL, owner });
service.listen(Number(env('PORT', '8092')));
void registerUntilDone({ url: env('QUACK_SCHEDULES_URL'), token: env('QUACK_SCHEDULES_TOKEN') }, env('QUACK_SLEEPER_LEAGUE'));
// A week's lineup runs depend on its kickoffs, which Sleeper publishes near the week, so re-register every few hours (idempotent).
if (owner) {
  const schedules = { url: env('QUACK_SCHEDULES_URL'), token: env('QUACK_SCHEDULES_TOKEN') };
  const register = (fn: typeof registerLineupSchedules) => () => fn(schedules, client, env('QUACK_SLEEPER_LEAGUE'), owner).catch((err) => console.error(`${err.message}; will retry`));
  for (const job of [register(registerLineupSchedules), register(registerWaiversSchedules), register(registerFinderSchedules), register(registerTrendsSchedules), register(registerReportSchedules)]) {
    void job();
    setInterval(job, 6 * 60 * 60_000).unref();
  }
}
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => (service.stop(), process.exit(0)));
