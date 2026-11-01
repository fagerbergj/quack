import { fileURLToPath } from 'node:url';
import { createDistributor } from './server.ts';
import { processDriver } from './process-driver.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const DRAIN_MS = 20_000;
const runtime = fileURLToPath(new URL('../../runtime/src/main.ts', import.meta.url));
// shortcut: session processes share this container (A11's process driver); move to the Kata driver with A12.
const driver = processDriver({ command: [process.execPath, runtime], dataDir: env('QUACK_DATA_DIR'), agentsDir: process.env.QUACK_AGENTS_DIR, startTimeoutMs: 60_000 });
const server = createDistributor(driver, { webDir: process.env.QUACK_WEB_DIR }).listen(Number(env('PORT', '8080')));
for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  // Sessions stop first: their logs are durable, so a request cut here is answered after the restart. The deadline beats
  // compose's 30 s stop grace, so a stuck child or an attached viewer cannot hold the exit.
  process.on(signal, () => {
    setTimeout(() => process.exit(1), DRAIN_MS).unref();
    void Promise.all(driver.running().map((k) => driver.stop(k))).then(() => server.close(() => process.exit(0)));
  });
}
