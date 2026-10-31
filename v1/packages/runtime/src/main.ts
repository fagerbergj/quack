import { readFileSync } from 'node:fs';
import { buildModels, createSessionServer, loadAgent, openSession, parseDeployment } from '@quack/session';
import { catalog } from './catalog.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (value === undefined) throw new Error(`${name} is required`);
  return value;
}

function readJson(path: string) {
  try {
    return JSON.parse(readFileSync(path, 'utf8'));
  } catch (err) {
    throw new Error(`reading ${path}: ${err instanceof Error ? err.message : String(err)}`);
  }
}
const deployment = parseDeployment(readJson(env('QUACK_CONFIG')));
const agent = await loadAgent(env('QUACK_AGENT'), deployment, catalog);

const session = await openSession({ dir: env('QUACK_SESSION_DIR'), models: buildModels(deployment), agent });
const server = createSessionServer(session);
server.listen(Number(env('PORT', '8080')));

for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.on(signal, () => server.close(() => void session.close().then(() => process.exit(0))));
}
