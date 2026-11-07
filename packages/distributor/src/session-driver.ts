import type { networkInterfaces } from 'node:os';
import { sessionEnv } from '@quack/runtime';
import type { RubricFor } from './agent-bundle.ts';
import type { CredentialBroker } from './credentials.ts';
import { dockerApi, parseDockerHost, type DockerCall } from './docker-api.ts';
import { dockerLauncher, onSessionNetwork, reapSessions } from './docker-driver.ts';
import { parseHostConfig } from './host-config.ts';
import { reapOrphans } from './orphans.ts';
import { processDriver, sessionHost, type HostDriver } from './process-driver.ts';

type Options = { dataDir: string; agentsDir?: string; rubricFor?: RubricFor; runtime: string; sessionPort: number; broker: CredentialBroker; onExit?: (key: string) => void };
type Seams = { api?: DockerCall; interfaces?: ReturnType<typeof networkInterfaces> };
export type SessionDriver = { driver: HostDriver; /** Peers the public listener drops. */ refuse?: (address: string | undefined) => boolean; /** The one address the session listener binds. */ listenAddress: string; /** Settles once no previous distributor's session can still be running. */ ready: Promise<void> };

const LOOPBACK = '127.0.0.1';

/** QUACK_SESSION_DRIVER picks where sessions run: child processes here (default) or one gVisor/Kata container each. */
export async function sessionDriver(env: NodeJS.ProcessEnv, o: Options, seams: Seams = {}): Promise<SessionDriver> {
  const config = parseHostConfig(env);
  const { sessionPort, broker } = o;
  const base = { dataDir: o.dataDir, agentsDir: o.agentsDir, rubricFor: o.rubricFor, onExit: o.onExit };
  if (config.driver === 'process') {
    // shortcut: process sessions share this host, its secrets and its loopback, so one can still dial the public port as the tailnet; the docker driver fences it off.
    // Processes a previous run left are stopped before anything starts or the backfill opens a log they own.
    const reaped = reapOrphans(o.dataDir, { entry: o.runtime }).then((dirs) => void (dirs.length && console.warn(`stopped ${dirs.length} session processes a previous run left`)), (err) => console.error('could not stop old session processes:', err.message));
    return { listenAddress: LOOPBACK, ready: reaped, driver: processDriver({ ...base, command: [process.execPath, o.runtime], startTimeoutMs: 60_000, env: { QUACK_INDEX_URL: `http://${LOOPBACK}:${sessionPort}` }, broker, inherit: (agentPath) => sessionEnv(agentPath) }) };
  }
  if (Number(new URL(config.distributorUrl).port) !== sessionPort) throw new Error(`QUACK_SESSION_DISTRIBUTOR_URL must name the session port ${sessionPort}, never the public one`);
  const api = seams.api ?? dockerApi(parseDockerHost(config.dockerHost));
  const { refuse, address } = await onSessionNetwork(api, config.network, seams.interfaces);
  // Old containers go before the first launch, or the reap could remove a session this distributor just started.
  const reaped = reapSessions(api).then(() => {}, (err) => console.error('could not remove old session containers:', err.message));
  const launch = dockerLauncher({ ...config, api, broker, inherit: (agentPath) => sessionEnv(agentPath), startTimeoutMs: 60_000 });
  return { listenAddress: address, ready: reaped, driver: sessionHost(base, async (spec) => (await reaped, launch(spec))), refuse };
}
