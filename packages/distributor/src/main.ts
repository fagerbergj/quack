import { readFile } from 'node:fs/promises';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { readDeploymentFile } from '@quack/sdk';
import { fileURLToPath } from 'node:url';
import { catalogStore, eventsRoute, openCatalog } from '@quack/catalog';
import { parseDeployment, priceOf } from '@quack/session';
import { backfillCatalog } from './catalog-sync.ts';
import { createDistributor } from './server.ts';
import { openNotifications } from './notifications.ts';
import { pinnedAgentPath } from './process-driver.ts';
import { credentialBroker } from './credentials.ts';
import { sessionDriver } from './session-driver.ts';
import { crossSession, declaredApps, loadManifests } from '@quack/runtime/catalog';
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
// The listener sessions are given; the public PORT serves the reverse proxy and the tailnet.
const sessionPort = Number(env('QUACK_SESSION_PORT', String(port + 1)));
const dataDir = env('QUACK_DATA_DIR');
const deployment = readDeploymentFile();
// The catalog's store is the one the deployment file's catalog.store names, otherwise catalog.sqlite in the data dir.
const catalog = openCatalog({ dataDir, store: catalogStore(deployment) });
const broker = credentialBroker();
const manifests = await loadManifests();

// A process that exits, stopped or crashed, is the catalog's stopped; the session cannot report its own end.
const stopped = (key: string) => void catalog.stopped(key).catch((err) => console.error(`catalog: ${key} stop not recorded:`, err.message));
const { driver, refuse, listenAddress, ready } = await sessionDriver(process.env, { dataDir, agentsDir: process.env.QUACK_AGENTS_DIR, rubricFor: rubricFrom(manifests), runtime, sessionPort, broker, onExit: stopped });
// Before any session starts, so each log has one owner while its stream is seeded.
await ready;
const backfilled = await backfillCatalog(catalog, dataDir, priceOf(parseDeployment(deployment)));
if (backfilled !== undefined) console.log(`catalog: backfilled ${backfilled} sessions from their logs`);
// The extensions the deployment file lists run in this process, under /apps/<name>/*; their calls back here, and to each other, are in-process.
const mounted = await mountApps(deployment, manifests, { distributorUrl: 'http://distributor.local', distributor: (req, res) => server.emit('request', req, res) });
const pinnedDefinition = (key: string) => readFile(pinnedAgentPath(dataDir, key), 'utf8').then(JSON.parse, () => null);
const apps = appsGateway(mounted, broker.sessionOf, async (key) => declaredApps(await pinnedDefinition(key), manifests));
// VAPID keys are made on first boot under the data dir; QUACK_VAPID_PUBLIC_KEY, QUACK_VAPID_PRIVATE_KEY and QUACK_VAPID_SUBJECT override them.
const push = openNotifications({
  dataDir, env: process.env, title: async (key) => (await catalog.getSession(key))?.title,
  artifacts: async (key) => (await catalog.listArtifacts({ session: key, pageSize: 1000 })).rows,
});
// QUACK_PROXY_SECRET is the value the reverse proxy sends in x-quack-proxy-secret; without it no identity header is believed.
const sessions = { sessionOf: broker.sessionOf, crossSession: async (key: string) => crossSession(await pinnedDefinition(key)), events: eventsRoute(catalog, (key, events) => push.notify(key, events)) };
const routes = async (req: IncomingMessage, res: ServerResponse) => (await push.routes(req, res)) || (await apps(req, res)) || (await broker.route(req, res));
const server = createDistributor(driver, { refuse, sessions, catalog, routes, proxySecret: process.env.QUACK_PROXY_SECRET || undefined, webDir: process.env.QUACK_WEB_DIR }).listen(port);
server.sessions.listen(sessionPort, listenAddress);
// A failed start (an extension finding its agent unpromoted, say) exits so compose restarts it, as the separate service did.
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
