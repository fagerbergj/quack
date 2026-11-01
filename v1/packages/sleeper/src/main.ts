import { createSleeperClient } from './client.ts';
import { registerSnapshotSchedule } from './schedules.ts';
import { createSleeperService } from './service.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const service = createSleeperService({ serviceToken: env('QUACK_SLEEPER_TOKEN'), dbPath: env('QUACK_SLEEPER_DB', '/data/sleeper.db'), client: createSleeperClient() });
service.listen(Number(env('PORT', '8092')));
registerSnapshotSchedule({ url: env('QUACK_SCHEDULES_URL'), token: env('QUACK_SCHEDULES_TOKEN') }, env('QUACK_SLEEPER_LEAGUE')).catch((err) => console.error(err.message));
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => (service.stop(), process.exit(0)));
