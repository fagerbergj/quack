import { fileURLToPath } from 'node:url';
import { createDistributor } from './server.ts';
import { processDriver } from './process-driver.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const runtime = fileURLToPath(new URL('../../runtime/src/main.ts', import.meta.url));
// shortcut: session processes share this container (A11's process driver); move to the Kata driver with A12.
const driver = processDriver({ command: [process.execPath, runtime], dataDir: env('QUACK_DATA_DIR'), startTimeoutMs: 60_000 });
const server = createDistributor(driver).listen(Number(env('PORT', '8080')));
for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  // Sessions stop first: their logs are durable, so a request cut here is answered after the restart.
  process.on(signal, () => void Promise.all(driver.running().map((k) => driver.stop(k))).then(() => server.close(() => process.exit(0))));
}
