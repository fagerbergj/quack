import { randomBytes } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { indexRoutes, openStore } from '@quack/index';
import { createDistributor } from './server.ts';
import { processDriver } from './process-driver.ts';
import { sleeperRoutes } from './sleeper-routes.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const DRAIN_MS = 20_000;
const runtime = fileURLToPath(new URL('../../runtime/src/main.ts', import.meta.url));
// shortcut: session processes share this container (A11's process driver); move to the Kata driver with A12.
const port = Number(env('PORT', '8080'));
const dataDir = env('QUACK_DATA_DIR');
const token = randomBytes(24).toString('hex');
const { store } = openStore({ QUACK_INDEX_DATABASE_URL: process.env.QUACK_INDEX_DATABASE_URL, QUACK_DATA_DIR: dataDir });
void store.ensure().catch((err) => console.error('index migrations will retry:', err.message));
const sessionEnv = { QUACK_INDEX_URL: `http://127.0.0.1:${port}`, QUACK_INDEX_TOKEN: token };
const driver = processDriver({ command: [process.execPath, runtime], dataDir, agentsDir: process.env.QUACK_AGENTS_DIR, startTimeoutMs: 60_000, env: sessionEnv });
// Opt-in: with all three set, the page may start the lineup job without ever seeing the service token.
const sleeper = process.env.QUACK_SLEEPER_URL && process.env.QUACK_SLEEPER_TOKEN && process.env.QUACK_SLEEPER_LEAGUE
  ? sleeperRoutes({ url: process.env.QUACK_SLEEPER_URL, token: process.env.QUACK_SLEEPER_TOKEN, league: process.env.QUACK_SLEEPER_LEAGUE })
  : undefined;
const indexHttp = indexRoutes(store, token);
const server = createDistributor(driver, { webDir: process.env.QUACK_WEB_DIR, index: async (req, res) => (await indexHttp(req, res)) || (await sleeper?.(req, res)) || false }).listen(port);
for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  // Sessions stop first: their logs are durable, so a request cut here is answered after the restart. The deadline beats
  // compose's 30 s stop grace, so a stuck child or an attached viewer cannot hold the exit.
  process.on(signal, () => {
    setTimeout(() => process.exit(1), DRAIN_MS).unref();
    void server.drain().then(() => server.close(() => process.exit(0)));
  });
}
