import { readFileSync } from 'node:fs';
import { join } from 'node:path';

export type StoreEntry = { kind: 'sqlite' } | { kind: 'postgres'; urlEnv: string };
export type ServiceEntry = { url: string; tokenEnv?: string; store?: string; settings?: unknown };
/** The parts of the deployment file a service reads; the loader and `validate` check the whole shape. */
export type DeploymentFile = { providers?: Record<string, { slots?: number }>; stores?: Record<string, StoreEntry>; services?: Record<string, ServiceEntry> };
export type StoreChoice = { kind: 'sqlite' } | { kind: 'postgres'; url: string };

export function readDeploymentFile(env: NodeJS.ProcessEnv = process.env): DeploymentFile {
  const path = env.QUACK_CONFIG;
  if (!path) throw new Error('QUACK_CONFIG is required');
  try {
    return JSON.parse(readFileSync(path, 'utf8'));
  } catch (err) {
    throw new Error(`reading ${path}: ${err instanceof Error ? err.message : String(err)}`);
  }
}

export function serviceEntry(file: DeploymentFile, name: string): ServiceEntry {
  const entry = file.services?.[name];
  if (!entry) throw new Error(`services.${name} is missing from the deployment file`);
  return entry;
}

/** The bearer token a service entry names, read from the environment. */
export function serviceToken(entry: ServiceEntry, name: string, env: NodeJS.ProcessEnv = process.env): string {
  if (!entry.tokenEnv) throw new Error(`services.${name}.tokenEnv is required: ${name} authenticates its callers`);
  const token = env[entry.tokenEnv];
  if (!token) throw new Error(`services.${name}.tokenEnv ${entry.tokenEnv} is not set`);
  return token;
}

/** The store a name selects; no name takes a sqlite file. A postgres store reads its URL from the variable it names. */
export function storeFor(file: DeploymentFile, name: string | undefined, env: NodeJS.ProcessEnv = process.env): StoreChoice {
  const store = name === undefined ? { kind: 'sqlite' as const } : file.stores?.[name];
  if (!store) throw new Error(`store ${name} is missing from the deployment file`);
  if (store.kind === 'sqlite') return store;
  const url = env[store.urlEnv];
  if (!url) throw new Error(`stores.${name}.urlEnv ${store.urlEnv} is not set`);
  return { kind: 'postgres', url };
}

/** A SQLite service's database file: `<name>.db` in the data dir, unless its store is postgres, which these services do not speak yet. */
export function serviceDbPath(file: DeploymentFile, name: string, env: NodeJS.ProcessEnv = process.env): string {
  if (storeFor(file, serviceEntry(file, name).store, env).kind !== 'sqlite') throw new Error(`services.${name}.store is postgres; ${name} keeps SQLite only`);
  if (!env.QUACK_DATA_DIR) throw new Error('QUACK_DATA_DIR is required');
  return join(env.QUACK_DATA_DIR, `${name}.db`);
}

/** The service's settings after its own check; throws with every problem. */
export function checkedSettings<T>(name: string, settings: unknown, problems: (settings: unknown) => string[]): T {
  const found = problems(settings);
  if (found.length) throw new Error(`invalid services.${name}.settings:\n- ${found.join('\n- ')}`);
  return settings as T;
}
