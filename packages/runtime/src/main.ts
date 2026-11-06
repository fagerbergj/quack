import { readFileSync } from 'node:fs';
import { buildModels, createSessionServer, exitOnStop, listenReporting, declaredEnv, deploymentEnv, loadAgent, openSession, parseDeployment, priceOf, referencedProviders, resolveDeployment, sessionFacts, startLifecycleSender, takeLaunchToken } from '@quack/session';
import { startPublisher } from '@quack/index';
import { catalog, deploymentExtensions } from './catalog.ts';

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
// Taken first, before a resumed run can start a shell.
const token = await takeLaunchToken();
const config = parseDeployment(readJson(env('QUACK_CONFIG')));
const definition = readJson(env('QUACK_AGENT'));
// Only the providers this agent names are asked for their models, so one that is down cannot slow an agent that never uses it.
const deployment = await resolveDeployment(config, fetch, referencedProviders(definition, config));
const agent = await loadAgent(env('QUACK_AGENT'), deployment, catalog, deploymentExtensions);
const { name: agentName, version: agentVersion, extensions } = definition;

const session = await openSession({ dir: env('QUACK_SESSION_DIR'), models: buildModels(deployment), agent, timeZone: deployment.timeZone, exec: deployment.exec });
const publisher = process.env.QUACK_INDEX_URL && process.env.QUACK_INDEX_TOKEN
  ? await startPublisher(session, { url: process.env.QUACK_INDEX_URL, token: process.env.QUACK_INDEX_TOKEN, meta: { key: env('QUACK_SESSION_KEY'), agent: agentName, version: agentVersion }, priceOf: priceOf(deployment) })
  : undefined;
// The lifecycle channel feeds the session catalog; the index publisher keeps the index's reads current until the cutover.
const sender = process.env.QUACK_INDEX_URL && process.env.QUACK_INDEX_TOKEN
  ? await startLifecycleSender(session, {
    url: process.env.QUACK_INDEX_URL, token: process.env.QUACK_INDEX_TOKEN, key: env('QUACK_SESSION_KEY'), facts: () => sessionFacts(env('QUACK_SESSION_DIR')), priceOf: priceOf(deployment),
    lastMessage: async () => (await session.messages(8)).filter((m) => m.role === 'assistant').at(-1)?.text,
  })
  : undefined;
// Everything above read what it needs; dropping it keeps child processes (tool shells) from inheriting service secrets.
for (const name of [...declaredEnv(extensions, catalog), ...deploymentEnv(deploymentExtensions), 'QUACK_INDEX_URL', 'QUACK_INDEX_TOKEN']) delete process.env[name];
const server = createSessionServer(session, { token });
// Loopback unless the host says otherwise: a container listens on its own interface, where only its distributor can reach it.
listenReporting(server, Number(env('PORT', '8080')), env('QUACK_SESSION_HOST', '127.0.0.1'));

// A caller cut off mid-invocation re-sends the same requestId to rejoin the run.
exitOnStop(server, async () => {
  await Promise.all([publisher?.stop(), sender?.stop()]);
  await session.close();
});
