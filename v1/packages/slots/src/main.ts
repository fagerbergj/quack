import { readFileSync } from 'node:fs';
import { slotLimits } from './limits.ts';
import { createSlotsService } from './service.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const service = createSlotsService({
  serviceToken: env('QUACK_SLOTS_TOKEN'),
  dbPath: env('QUACK_SLOTS_DB', '/data/slots.db'),
  limits: slotLimits(JSON.parse(readFileSync(env('QUACK_CONFIG'), 'utf8'))),
});
service.listen(Number(env('PORT', '8092')));
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => service.close(() => process.exit(0)));
