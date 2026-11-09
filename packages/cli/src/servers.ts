// Saved servers: the distributors `quack server add` remembers, in $XDG_CONFIG_HOME/quack/config.json.
import { mkdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';

type Env = Record<string, string | undefined>;
export type Saved = { url: string; token?: string };
export type Config = { current?: string; servers: Record<string, Saved> };
/** Where a command sends its requests; `from` is the saved server's name, or which setting gave the URL. */
export type Target = { url: string; token?: string; from: string; warning?: string };

export class ConfigError extends Error {}

export const DEFAULT_URL = 'http://localhost:8080';

export const configPath = (env: Env) => join(env.XDG_CONFIG_HOME || join(env.HOME || homedir(), '.config'), 'quack', 'config.json');

const valid = (c: any): c is Config =>
  !!c && typeof c.servers === 'object' && !Array.isArray(c.servers) && (c.current === undefined || typeof c.current === 'string') &&
  Object.values(c.servers).every((s: any) => typeof s?.url === 'string' && (s.token === undefined || typeof s.token === 'string'));

/** A missing file reads as no servers; one that cannot be read or parsed is a ConfigError naming the file. */
export async function readConfig(path: string): Promise<Config> {
  let text: string;
  try {
    text = await readFile(path, 'utf8');
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === 'ENOENT') return { servers: {} };
    throw new ConfigError(`cannot read ${path}: ${(e as Error).message}`);
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {}
  if (!valid(parsed)) throw new ConfigError(`${path} is not a quack config; fix or delete it`);
  return parsed;
}

// The rename keeps a reader from seeing half a file; the temp file is created 0600 because it can hold a token.
export async function writeConfig(path: string, config: Config) {
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const tmp = `${path}.${process.pid}.tmp`;
  await rm(tmp, { force: true });
  await writeFile(tmp, `${JSON.stringify(config, null, 2)}\n`, { mode: 0o600, flag: 'wx' });
  await rename(tmp, path);
}

const saved = (config: Config, name: string): Target => {
  const s = Object.hasOwn(config.servers, name) ? config.servers[name] : undefined;
  if (!s) throw new ConfigError(`no saved server ${name}; see quack server list`);
  return { url: s.url, token: s.token, from: name };
};

/** --url, then --server, then $QUACK_URL, then the current saved server, then localhost; flags beat the environment. */
export async function target(flags: { url?: string; server?: string }, env: Env): Promise<Target> {
  const envToken = env.QUACK_ADMIN_TOKEN;
  if (flags.url) return { url: flags.url, token: envToken, from: '--url' };
  if (flags.server) return withEnvToken(saved(await readConfig(configPath(env)), flags.server), envToken);
  if (env.QUACK_URL) return { url: env.QUACK_URL, token: envToken, from: '$QUACK_URL' };
  const fallback = { url: DEFAULT_URL, token: envToken, from: 'default' };
  try {
    const config = await readConfig(configPath(env));
    return config.current ? withEnvToken(saved(config, config.current), envToken) : fallback;
  } catch (e) {
    if (!(e instanceof ConfigError)) throw e;
    return { ...fallback, warning: `${e.message}; using ${DEFAULT_URL}` };
  }
}

// A saved token belongs to its server; $QUACK_ADMIN_TOKEN still covers a server saved without one.
const withEnvToken = (t: Target, envToken?: string): Target => ({ ...t, token: t.token ?? envToken });
