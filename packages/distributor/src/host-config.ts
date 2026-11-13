import { isIP } from 'node:net';

export type HostConfig =
  | { driver: 'process' }
  | { driver: 'docker'; image: string; runtime: string; network: string; data: { volume: string } | { hostDir: string }; distributorUrl: string; egressProxy: string; dockerHost: string; passthrough: string[] };

const need = (env: NodeJS.ProcessEnv, name: string) => {
  const value = env[name];
  if (!value) throw new Error(`${name} is required when QUACK_SESSION_DRIVER=docker`);
  return value;
};

// Session containers have no DNS (gVisor cannot reach Docker's resolver, and the proxy resolves for them), so both of their hosts are IP literals.
function byAddress(env: NodeJS.ProcessEnv, name: string) {
  const value = need(env, name).replace(/\/$/, '');
  if (!/^https?:\/\//.test(value)) throw new Error(`${name} must be an http(s) URL the session containers can reach`);
  if (!isIP(new URL(value).hostname.replace(/^\[|\]$/g, ''))) throw new Error(`${name} must name its host by IP address: session containers have no DNS`);
  return value;
}

// A named volume needs no migration (each session mounts its own subpath), but needs Docker Engine 26 or newer.
function dataMount(env: NodeJS.ProcessEnv): { volume: string } | { hostDir: string } {
  const [volume, hostDir] = [env.QUACK_DATA_VOLUME, env.QUACK_HOST_DATA_DIR];
  if (volume && hostDir) throw new Error('set QUACK_DATA_VOLUME or QUACK_HOST_DATA_DIR, not both');
  if (volume) return { volume };
  if (!hostDir) throw new Error('QUACK_DATA_VOLUME (the data volume\'s Docker name) or QUACK_HOST_DATA_DIR is required when QUACK_SESSION_DRIVER=docker');
  if (!hostDir.startsWith('/')) throw new Error('QUACK_HOST_DATA_DIR must be an absolute path on the Docker host');
  return { hostDir };
}

/** Reads QUACK_SESSION_DRIVER (process, the default, or docker) and, for docker, the settings it needs; throws on anything unusable. */
export function parseHostConfig(env: NodeJS.ProcessEnv): HostConfig {
  const driver = env.QUACK_SESSION_DRIVER ?? 'process';
  if (driver === 'process') return { driver };
  if (driver !== 'docker') throw new Error(`QUACK_SESSION_DRIVER must be process or docker, not ${driver}`);
  const runtime = env.QUACK_SESSION_RUNTIME ?? 'runsc';
  // runc shares the host kernel with the session's shell, which defeats the driver; it is for dev and tests only.
  if (runtime === 'runc') {
    if (env.QUACK_ALLOW_RUNC !== '1') throw new Error('QUACK_SESSION_RUNTIME=runc gives sessions the host kernel; set QUACK_ALLOW_RUNC=1 for dev and tests only');
    console.warn('WARNING: session containers run under runc, not gVisor or Kata. Do not use this in production.');
  }
  const data = dataMount(env);
  const distributorUrl = byAddress(env, 'QUACK_SESSION_DISTRIBUTOR_URL');
  return {
    driver, runtime, data, distributorUrl,
    egressProxy: byAddress(env, 'QUACK_SESSION_EGRESS_PROXY'),
    image: need(env, 'QUACK_SESSION_IMAGE'),
    network: env.QUACK_SESSION_NETWORK ?? 'quack-sessions',
    dockerHost: env.QUACK_DOCKER_HOST ?? '/var/run/docker.sock',
    passthrough: (env.QUACK_SESSION_PASSTHROUGH ?? '').split(',').map((s) => s.trim()).filter(Boolean),
  };
}
