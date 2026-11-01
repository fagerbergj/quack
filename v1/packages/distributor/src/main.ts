import { randomBytes } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { createStore, indexRoutes } from '@quack/index';
import pg from 'pg';
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
const port = Number(env('PORT', '8080'));
// The index is off without a database URL; sessions then get no publish target and keep their facts queued in their logs (A6).
const dbUrl = process.env.QUACK_INDEX_DATABASE_URL;
const token = randomBytes(24).toString('hex');
const store = dbUrl ? createStore(new pg.Pool({ connectionString: dbUrl })) : undefined;
void store?.ensure().catch((err) => console.error('index migrations will retry:', err.message));
const sessionEnv = store ? { QUACK_INDEX_URL: `http://127.0.0.1:${port}`, QUACK_INDEX_TOKEN: token } : {};
const driver = processDriver({ command: [process.execPath, runtime], dataDir: env('QUACK_DATA_DIR'), agentsDir: process.env.QUACK_AGENTS_DIR, startTimeoutMs: 60_000, env: sessionEnv });
const server = createDistributor(driver, { webDir: process.env.QUACK_WEB_DIR, index: store && indexRoutes(store, token) }).listen(port);
for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  // Sessions stop first: their logs are durable, so a request cut here is answered after the restart. The deadline beats
  // compose's 30 s stop grace, so a stuck child or an attached viewer cannot hold the exit.
  process.on(signal, () => {
    setTimeout(() => process.exit(1), DRAIN_MS).unref();
    void server.drain().then(() => server.close(() => process.exit(0)));
  });
}
