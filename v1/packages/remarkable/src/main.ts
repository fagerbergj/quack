import { createRmClient } from './client.ts';
import { createRemarkableService } from './service.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const service = createRemarkableService({
  serviceToken: env('QUACK_REMARKABLE_TOKEN'),
  dbPath: env('QUACK_REMARKABLE_DB', '/data/remarkable.db'),
  distributorUrl: env('QUACK_DISTRIBUTOR_URL'),
  agent: env('QUACK_REMARKABLE_AGENT', 'remarkable-ingest'),
  client: createRmClient({ url: env('RMFAKECLOUD_URL'), email: env('RMFAKECLOUD_EMAIL'), password: env('RMFAKECLOUD_PASSWORD') }),
});
service.listen(Number(env('PORT', '8093')));
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => (service.stop(), process.exit(0)));
