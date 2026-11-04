import { readFileSync } from 'node:fs';
import { join } from 'node:path';

export type StoreEntry = { kind: 'sqlite' } | { kind: 'postgres'; urlEnv: string };
export type ExtensionEntry = { store?: string; settings?: unknown };
/** The parts of the deployment file an extension reads; the loader and `validate` check the whole shape. */
export type DeploymentFile = { providers?: Record<string, { slots?: number }>; stores?: Record<string, StoreEntry>; extensions?: Record<string, ExtensionEntry> };
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

export function extensionEntry(file: DeploymentFile, name: string): ExtensionEntry {
  const entry = file.extensions?.[name];
  if (!entry) throw new Error(`extensions.${name} is missing from the deployment file`);
  return entry;
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

/** A SQLite extension's database file: `<name>.db` in the data dir, unless its store is postgres, which these services do not speak yet. */
export function extensionDbPath(file: DeploymentFile, name: string, env: NodeJS.ProcessEnv = process.env): string {
  if (storeFor(file, extensionEntry(file, name).store, env).kind !== 'sqlite') throw new Error(`extensions.${name}.store is postgres; ${name} keeps SQLite only`);
  if (!env.QUACK_DATA_DIR) throw new Error('QUACK_DATA_DIR is required');
  return join(env.QUACK_DATA_DIR, `${name}.db`);
}

/** The extension's settings after its own check; throws with every problem. */
export function checkedSettings<T>(name: string, settings: unknown, problems: (settings: unknown) => string[]): T {
  const found = problems(settings);
  if (found.length) throw new Error(`invalid extensions.${name}.settings:\n- ${found.join('\n- ')}`);
  return settings as T;
}
