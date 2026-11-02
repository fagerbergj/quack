import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import type { Extension } from '@earendil-works/pi-durable';
import type { ContributedWorkflows, ExtensionManifest } from '@quack/sdk';
import type { Catalog, CatalogEntry, DeploymentExtensions } from '@quack/session';

const root = fileURLToPath(new URL('..', import.meta.url));

/** The one list of extension modules this image carries: `quack.extensions` in the runtime package.json, packages or paths from its root. */
export const extensionList = (): string[] => JSON.parse(readFileSync(join(root, 'package.json'), 'utf8')).quack.extensions;

export async function loadManifests(list: string[] = extensionList()): Promise<ExtensionManifest[]> {
  return Promise.all(list.map(async (spec) => (await import(spec.startsWith('.') ? pathToFileURL(join(root, spec)).href : spec)).manifest as ExtensionManifest));
}

const entry = (m: ExtensionManifest, workflows: ContributedWorkflows): CatalogEntry => {
  const { env } = m;
  return Object.assign((config: unknown) => m.session!(config, workflows), { env: typeof env === 'function' ? (config: unknown) => env(config, workflows) : env, requires: m.requires, optional: m.optional });
};

/** Entries by the name agent definitions select them with (A8), and the extensions every session gets, from the manifests. */
export function buildCatalog(manifests: ExtensionManifest[]): { catalog: Catalog; deploymentExtensions: DeploymentExtensions } {
  const known = new Set(manifests.map((m) => m.name));
  for (const m of manifests) for (const r of m.requires ?? []) if (!known.has(r)) throw new Error(`extension ${m.name} requires ${r}, which this image does not carry`);
  const workflows: ContributedWorkflows = Object.assign({}, ...manifests.map((m) => m.workflows));
  const withSession = manifests.filter((m) => m.session);
  const always = withSession.filter((m) => m.deployment).map((m) => Object.assign(() => m.session!(undefined, workflows) as Extension[], { env: m.env as readonly string[] | undefined }));
  return { catalog: Object.fromEntries(withSession.filter((m) => !m.deployment).map((m) => [m.name, entry(m, workflows)])), deploymentExtensions: always };
}

export const { catalog, deploymentExtensions } = buildCatalog(await loadManifests());
