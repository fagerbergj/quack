import { createSchedulesService } from './service.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const service = createSchedulesService({
  serviceToken: env('QUACK_SCHEDULES_TOKEN'),
  distributorUrl: env('QUACK_DISTRIBUTOR_URL'),
  dbPath: env('QUACK_SCHEDULES_DB', '/data/schedules.db'),
  agentsDir: process.env.QUACK_AGENTS_DIR,
});
service.listen(Number(env('PORT', '8091')));
service.start();
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => (service.stop(), process.exit(0)));
