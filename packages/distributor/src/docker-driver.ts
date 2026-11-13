import { BlockList, isIP, isIPv4 } from 'node:net';
import { networkInterfaces } from 'node:os';
import { readFile, writeFile } from 'node:fs/promises';
import { basename, join } from 'node:path';
import { shellEnv } from '@quack/session';
import type { CredentialBroker } from './credentials.ts';
import type { DockerCall } from './docker-api.ts';
import { SESSION_LABEL, SESSION_MOUNT, SESSION_PREFIX, sessionMount } from './docker-gate.ts';
import { proxyKeyedServices, proxyProviders } from './keyed-services.ts';
import { LAUNCH_TOKEN_FILE, LISTENER_TOKEN_FILE, writeLaunchToken, writeListenerToken, type Launch } from './process-driver.ts';

const SESSION_PORT = 8080;
const STOP_GRACE_SECONDS = 20;
const PING_TIMEOUT_MS = 2_000;

export type DockerLauncherOptions = {
  api: DockerCall;
  broker: CredentialBroker;
  image: string;
  /** OCI runtime: runsc (gVisor) or a Kata runtime; runc shares the host kernel and is for dev and tests. */
  runtime: string;
  network: string;
  /** The data volume holds each session's directory: a named volume (each session mounts its subpath; Docker 26+) or a host path (bind). */
  data: { volume: string } | { hostDir: string };
  /** What a session container uses to reach this distributor's session listener (QUACK_SESSION_PORT) by its fixed address on the session network, e.g. http://10.213.0.2:8081. */
  distributorUrl: string;
  /** The session network's only way out, e.g. http://10.213.0.3:3128; without it a session on an internal network reaches nothing beyond it. */
  egressProxy?: string;
  inherit: (agentPath: string | undefined) => NodeJS.ProcessEnv | Promise<NodeJS.ProcessEnv>;
  /** Variables copied into containers as they are; these are secrets the container then holds until they are proxied. */
  passthrough?: string[];
  command?: string[];
  startTimeoutMs?: number;
  /** Test seams: the session's address from its container name, and the pause between readiness probes. */
  urlFor?: (name: string) => string;
  sleep?: (ms: number) => Promise<void>;
};

/** The environment a session container gets: toolchain basics, the distributor and its launch credential, session paths, and nothing else unless passed through. */
export function containerEnv(inherited: NodeJS.ProcessEnv, o: { key: string; token: string; broker: CredentialBroker; distributorUrl: string; egressProxy?: string; passthrough: string[]; agentPath?: string; dir: string }): { env: Record<string, string>; dropped: string[] } {
  const env: Record<string, string> = { ...shellEnv(inherited), QUACK_SESSION_LISTENER_URL: o.distributorUrl, QUACK_LAUNCH_TOKEN_FILE: `${SESSION_MOUNT}/${LISTENER_TOKEN_FILE}` };
  Object.assign(env, proxyKeyedServices(inherited, { broker: o.broker, distributorUrl: o.distributorUrl, token: o.token }));
  for (const name of o.passthrough) if (inherited[name] !== undefined) env[name] = inherited[name]!;
  const dropped = Object.keys(inherited).filter((n) => !(n in env) && !n.startsWith('QUACK_CONFIG'));
  Object.assign(env, { PORT: String(SESSION_PORT), QUACK_SESSION_HOST: '0.0.0.0', QUACK_SESSION_TOKEN_FILE: `${SESSION_MOUNT}/${LAUNCH_TOKEN_FILE}`, QUACK_SESSION_DIR: SESSION_MOUNT, QUACK_SESSION_KEY: o.key });
  if (o.agentPath) env.QUACK_AGENT = o.agentPath.replace(o.dir, SESSION_MOUNT);
  if (inherited.QUACK_CONFIG) env.QUACK_CONFIG = `${SESSION_MOUNT}/deployment.json`;
  if (o.egressProxy) Object.assign(env, proxyEnv(o.egressProxy, new URL(o.distributorUrl).hostname));
  return { env, dropped };
}

// git and curl read the lower-case names, Node and Go the upper-case ones; Node's fetch and http only with NODE_USE_ENV_PROXY.
function proxyEnv(proxy: string, distributorHost: string): Record<string, string> {
  const direct = `${distributorHost},localhost,127.0.0.1`;
  return { HTTP_PROXY: proxy, HTTPS_PROXY: proxy, http_proxy: proxy, https_proxy: proxy, NO_PROXY: direct, no_proxy: direct, NODE_USE_ENV_PROXY: '1' };
}

async function readConfig(path: string) {
  try {
    return JSON.parse(await readFile(path, 'utf8'));
  } catch (err) {
    throw new Error(`QUACK_CONFIG ${path} is unreadable: ${err instanceof Error ? err.message : String(err)}`);
  }
}

/** The Docker driver: one container per session from the quack image, with only that session's directory mounted. */
export function dockerLauncher(opts: DockerLauncherOptions): Launch {
  const { api } = opts;
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  const warned = new Set<string>();

  const call = async (method: 'GET' | 'POST' | 'DELETE', path: string, body: unknown, ok: number[]) => {
    const res = await api(method, path, body);
    if (!ok.includes(res.status)) throw new Error(`docker ${method} ${path.split('?')[0]} answered ${res.status}: ${JSON.stringify(res.body)}`);
    return res;
  };

  // Probes /ping until it answers; a container that exits first fails the start at once instead of at the timeout.
  async function ready(url: string, token: string, name: string, gone: Promise<unknown>) {
    let dead = false;
    void gone.then(() => (dead = true));
    const deadline = Date.now() + (opts.startTimeoutMs ?? 60_000);
    while (!dead) {
      if (await fetch(`${url}/ping`, { headers: { authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(PING_TIMEOUT_MS) }).then((r) => r.ok, () => false)) return;
      if (Date.now() > deadline) throw new Error(`session container ${name} did not answer /ping`);
      await sleep(100);
    }
    throw new Error(`session container ${name} exited during start; see docker logs ${name}`);
  }

  return async ({ key, dir, agentPath }) => {
    const name = `${SESSION_PREFIX}${basename(dir)}`;
    const token = opts.broker.mint(key);
    try {
      const inherited = await opts.inherit(agentPath);
      const { env, dropped } = containerEnv(inherited, { key, token, broker: opts.broker, distributorUrl: opts.distributorUrl, egressProxy: opts.egressProxy, passthrough: opts.passthrough ?? [], agentPath, dir });
      // Rewritten on every start, so a session that edits its copy keeps the change only until it next restarts.
      if (inherited.QUACK_CONFIG) {
        const proxied = proxyProviders(await readConfig(inherited.QUACK_CONFIG), inherited, { broker: opts.broker, distributorUrl: opts.distributorUrl, token });
        Object.assign(env, proxied.env);
        await writeFile(join(dir, 'deployment.json'), JSON.stringify(proxied.config));
      }
      const fresh = dropped.filter((n) => !(n in env) && !warned.has(n));
      if (fresh.length) console.warn(`docker driver: not passing ${fresh.join(', ')} to session containers; list a name in QUACK_SESSION_PASSTHROUGH to pass it`);
      fresh.forEach((n) => warned.add(n));
      await api('DELETE', `/containers/${name}?force=1`);
      // Written once any old container is gone, so only this launch can read it.
      const serverToken = await writeLaunchToken(dir);
      await writeListenerToken(dir, token);
      await call('POST', `/containers/create?name=${name}`, {
        Image: opts.image,
        Cmd: opts.command ?? ['node', 'packages/runtime/src/main.ts'],
        WorkingDir: '/app',
        Env: Object.entries(env).map(([k, v]) => `${k}=${v}`),
        Labels: { [SESSION_LABEL]: key },
        HostConfig: { Runtime: opts.runtime, NetworkMode: opts.network, ...sessionMount(opts.data, basename(dir)), AutoRemove: true, CapDrop: ['ALL'], SecurityOpt: ['no-new-privileges'] },
      }, [201]);
      // Waits for removal, which AutoRemove does after exit, so the name is free again when it resolves.
      const gone = api('POST', `/containers/${name}/wait?condition=removed`).catch(() => undefined);
      void gone.then(() => opts.broker.revoke(token));
      await call('POST', `/containers/${name}/start`, undefined, [204]);
      const url = (opts.urlFor ?? ((n) => `http://${n}:${SESSION_PORT}`))(name);
      await ready(url, serverToken, name, gone).catch(async (err) => {
        await api('POST', `/containers/${name}/stop?t=0`);
        throw err;
      });
      return {
        url,
        token: serverToken,
        onExit: (cb) => void gone.then(cb),
        stop: async () => {
          await api('POST', `/containers/${name}/stop?t=${STOP_GRACE_SECONDS}`);
          await gone;
        },
      };
    } catch (err) {
      opts.broker.revoke(token);
      throw err;
    }
  };
}

// Internal drops the default route; an isolated gateway leaves the bridge without a host address, or sessions could dial the host's own services.
function unfenced(net: { Internal?: boolean; EnableIPv6?: boolean; Options?: Record<string, string> } | null) {
  const isolated = (family: 4 | 6) => net?.Options?.[`com.docker.network.bridge.gateway_mode_ipv${family}`] === 'isolated';
  return !net?.Internal || !isolated(4) || (!!net.EnableIPv6 && !isolated(6));
}

/** Whether a peer is on the session network, for the public listener to drop, and this host's address there, the only one the session listener binds; throws when either is missing, as sessions must not boot unfenced. */
export async function onSessionNetwork(api: DockerCall, network: string, interfaces = networkInterfaces()): Promise<{ refuse: (address: string | undefined) => boolean; address: string }> {
  const { status, body } = await api('GET', `/networks/${encodeURIComponent(network)}`);
  const subnets = ((body as { IPAM?: { Config?: Array<{ Subnet?: string }> } } | null)?.IPAM?.Config ?? []).flatMap((c) => (c.Subnet ? [c.Subnet] : []));
  if (status !== 200 || !subnets.length) throw new Error(`docker network ${network} has no subnet to fence off (answered ${status})`);
  if (unfenced(body as Parameters<typeof unfenced>[0])) throw new Error(`docker network ${network} must be internal with an isolated gateway (docker network create --internal -o com.docker.network.bridge.gateway_mode_ipv4=isolated)`);
  const list = new BlockList();
  for (const subnet of subnets) {
    const [net, prefix] = subnet.split('/');
    list.addSubnet(net!, Number(prefix), isIPv4(net!) ? 'ipv4' : 'ipv6');
  }
  // A dual-stack listener reports IPv4 peers as ::ffff:a.b.c.d.
  const refuse = (address = '') => {
    const v4 = address.replace(/^::ffff:/, '');
    return isIPv4(v4) ? list.check(v4, 'ipv4') : isIP(address) === 6 && list.check(address, 'ipv6');
  };
  const address = Object.values(interfaces).flat().find((i) => i && refuse(i.address))?.address;
  if (!address) throw new Error(`this host has no address on docker network ${network} (${subnets.join(', ')}); attach the distributor's container to it`);
  return { refuse, address };
}

/** Removes containers a previous distributor left running; a restart does not adopt them. */
export async function reapSessions(api: DockerCall) {
  const filters = encodeURIComponent(JSON.stringify({ label: [SESSION_LABEL] }));
  const { body } = await api('GET', `/containers/json?all=1&filters=${filters}`);
  for (const c of Array.isArray(body) ? (body as Array<{ Id: string }>) : []) await api('DELETE', `/containers/${c.Id}?force=1`);
}
