import { createSlotsService } from './service.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const service = createSlotsService({
  serviceToken: env('QUACK_SLOTS_TOKEN'),
  dbPath: env('QUACK_SLOTS_DB', '/data/slots.db'),
  limits: JSON.parse(env('QUACK_SLOTS_LIMITS', '{}')),
});
service.listen(Number(env('PORT', '8092')));
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => service.close(() => process.exit(0)));
