import { readFileSync } from 'node:fs';
import { agentSpec, buildModels, createSessionServer, openSession, parseDefinition, parseDeployment } from '@quack/session';
import { catalog } from './catalog.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (value === undefined) throw new Error(`${name} is required`);
  return value;
}

const readJson = (path: string) => JSON.parse(readFileSync(path, 'utf8'));
const deployment = parseDeployment(readJson(env('QUACK_CONFIG')));
const definition = parseDefinition(readJson(env('QUACK_AGENT')), deployment, catalog);

const session = await openSession({ dir: env('QUACK_SESSION_DIR'), models: buildModels(deployment), agent: agentSpec(definition, catalog) });
const server = createSessionServer(session);
server.listen(Number(env('PORT', '8080')));

for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.on(signal, () => server.close(() => void session.close().then(() => process.exit(0))));
}
