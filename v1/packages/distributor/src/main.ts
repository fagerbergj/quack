import { readFile } from 'node:fs/promises';
import { storeFor, readDeploymentFile } from '@quack/sdk';
import { fileURLToPath } from 'node:url';
import { indexRoutes, openPush, openStore } from '@quack/index';
import { createDistributor, recordTurns } from './server.ts';
import { pinnedAgentPath, processDriver, sessionHost, type HostDriver } from './process-driver.ts';
import { credentialBroker } from './credentials.ts';
import { dockerApi, parseDockerHost } from './docker-api.ts';
import { dockerLauncher, reapSessions } from './docker-driver.ts';
import { parseHostConfig } from './host-config.ts';
import { sessionEnv } from '@quack/runtime';
import { declaredApps, loadManifests } from '@quack/runtime/catalog';
import { rubricFrom } from './rubrics.ts';
import { appsGateway, mountApps } from './apps.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (!value) throw new Error(`${name} is required`);
  return value;
}

const DRAIN_MS = 20_000;
const runtime = fileURLToPath(new URL('../../runtime/src/main.ts', import.meta.url));
const port = Number(env('PORT', '8080'));
const dataDir = env('QUACK_DATA_DIR');
// The index keeps a store named `index` when the deployment file has one, otherwise a sqlite file in the data dir.
const deployment = readDeploymentFile();
const { store } = openStore({ dataDir, store: storeFor(deployment, deployment.stores?.index && 'index') });
void store.ensure().catch((err) => console.error('index migrations will retry:', err.message));
const broker = credentialBroker();
const manifests = await loadManifests();

// QUACK_SESSION_DRIVER picks where sessions run: child processes here (default) or one gVisor/Kata container each.
function sessionDriver(): HostDriver {
  const config = parseHostConfig(process.env);
  const base = { dataDir, agentsDir: process.env.QUACK_AGENTS_DIR, rubricFor: rubricFrom(manifests) };
  if (config.driver === 'process') {
    // shortcut: process sessions share this container and its secrets; the docker driver is the isolated one.
    return processDriver({ ...base, command: [process.execPath, runtime], startTimeoutMs: 60_000, env: { QUACK_INDEX_URL: `http://127.0.0.1:${port}` }, broker, inherit: (agentPath) => sessionEnv(agentPath) });
  }
  const api = dockerApi(parseDockerHost(config.dockerHost));
  // Old containers go before the first launch, or the reap could remove a session this distributor just started.
  const reaped = reapSessions(api).catch((err) => console.error('could not remove old session containers:', err.message));
  const launch = dockerLauncher({ ...config, api, broker, inherit: (agentPath) => sessionEnv(agentPath), startTimeoutMs: 60_000 });
  return sessionHost(base, async (spec) => (await reaped, launch(spec)));
}
const driver = sessionDriver();
// The extensions the deployment file lists run in this process, under /apps/<name>/*; their calls back here, and to each other, are in-process.
const mounted = await mountApps(deployment, manifests, { distributorUrl: 'http://distributor.local', distributor: (req, res) => server.emit('request', req, res) });
const pinnedDefinition = (key: string) => readFile(pinnedAgentPath(dataDir, key), 'utf8').then(JSON.parse, () => null);
const apps = appsGateway(mounted, broker.sessionOf, async (key) => declaredApps(await pinnedDefinition(key), manifests));
// The page's rename, archive and delete always work; a token set also lets the retention sweep delete idle sessions.
const retire = { token: process.env.QUACK_ADMIN_TOKEN, waiting: (key: string) => store.waiting(key), forget: (key: string) => store.forget(key) };
// VAPID keys are made on first boot under the data dir; QUACK_VAPID_PUBLIC_KEY, QUACK_VAPID_PRIVATE_KEY and QUACK_VAPID_SUBJECT override them.
const push = openPush({ dataDir, env: process.env, artifacts: async (key) => (await store.artifacts()).filter((a) => a.session === key).map((a) => ({ kind: String(a.kind), version: Number(a.version), verdict: Boolean(a.verdict) })), title: async (key) => (await driver.sessions()).find((s) => s.key === key)?.title });
const turns = recordTurns(driver);
const indexHttp = indexRoutes(store, broker.sessionOf, (session, facts) => Promise.all([turns(session, facts), push.notify(session, facts)]).then(() => {}));
const server = createDistributor(driver, { retire, spend: () => store.sessions(), webDir: process.env.QUACK_WEB_DIR, index: async (req, res) => (await indexHttp(req, res)) || (await push.routes(req, res)) || (await apps(req, res)) || (await broker.route(req, res)) || false }).listen(port);
// A failed start (the GitHub app finding its agent unpromoted, say) exits so compose restarts it, as the separate service did.
for (const [name, app] of Object.entries(mounted)) {
  void Promise.resolve(app.start?.()).catch((err) => {
    console.error(`app ${name} failed to start:`, err);
    process.exit(1);
  });
}
const drain = async () => {
  await Promise.all(Object.values(mounted).map((app) => app.stop?.()));
  await server.drain();
};
for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  // Apps stop first so their loops end, then sessions: their logs are durable, so a request cut here is answered after the restart.
  // The deadline beats compose's 30 s stop grace, so a stuck child or an attached viewer cannot hold the exit.
  process.on(signal, () => {
    setTimeout(() => process.exit(1), DRAIN_MS).unref();
    void drain().then(() => server.close(() => process.exit(0)));
  });
}
