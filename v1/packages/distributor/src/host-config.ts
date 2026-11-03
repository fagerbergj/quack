export type HostConfig =
  | { driver: 'process' }
  | { driver: 'docker'; image: string; runtime: string; network: string; hostDataDir: string; distributorUrl: string; dockerHost: string; passthrough: string[] };

const need = (env: NodeJS.ProcessEnv, name: string) => {
  const value = env[name];
  if (!value) throw new Error(`${name} is required when QUACK_SESSION_DRIVER=docker`);
  return value;
};

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
  const hostDataDir = need(env, 'QUACK_HOST_DATA_DIR');
  if (!hostDataDir.startsWith('/')) throw new Error('QUACK_HOST_DATA_DIR must be an absolute path on the Docker host');
  const distributorUrl = need(env, 'QUACK_SESSION_DISTRIBUTOR_URL').replace(/\/$/, '');
  if (!/^https?:\/\//.test(distributorUrl)) throw new Error('QUACK_SESSION_DISTRIBUTOR_URL must be an http(s) URL the session containers can reach');
  return {
    driver, runtime, hostDataDir, distributorUrl,
    image: need(env, 'QUACK_SESSION_IMAGE'),
    network: env.QUACK_SESSION_NETWORK ?? 'quack-sessions',
    dockerHost: env.QUACK_DOCKER_HOST ?? '/var/run/docker.sock',
    passthrough: (env.QUACK_SESSION_PASSTHROUGH ?? '').split(',').map((s) => s.trim()).filter(Boolean),
  };
}
