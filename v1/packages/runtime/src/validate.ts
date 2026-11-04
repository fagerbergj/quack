import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { declaredEnv, deploymentEnv, loadAgent, parseDeployment, resolveDeployment, type DeploymentConfig } from '@quack/session';
import type { ExtensionManifest } from '@quack/sdk';
import { catalog, deploymentExtensions, manifests } from './catalog.ts';

export interface Validation { loaded: number; failures: string[] }

const subdirs = (root: string) => (existsSync(root) ? readdirSync(root, { withFileTypes: true }).filter((d) => d.isDirectory()).map((d) => join(root, d.name)) : []);

/** Each service's settings against the check its manifest publishes; the service runs the same check at boot. */
export function serviceSettingsProblems(services: DeploymentConfig['services'], list: ExtensionManifest[] = manifests): string[] {
  return Object.entries(services ?? {}).flatMap(([name, s]) => {
    const check = list.find((m) => m.name === name)?.settings;
    if (check) return check(s.settings).map((p) => `services.${name}.settings: ${p}`);
    return s.settings && Object.keys(s.settings).length ? [`services.${name}.settings: no extension named ${name} publishes settings`] : [];
  });
}

/** Loads every bundle under agentsDir and every session's pinned agent-def/agent.json under dataDir the way main.ts does. */
export async function validate(opts: { config: string; agentsDir: string; dataDir: string; fetch?: typeof fetch }): Promise<Validation> {
  // Models are listed from the providers; a provider that cannot be reached falls back to its hand-written entries.
  const deployment = await resolveDeployment(parseDeployment(JSON.parse(readFileSync(opts.config, 'utf8'))), opts.fetch);
  const settingsProblems = serviceSettingsProblems(deployment.services);
  if (settingsProblems.length) throw new Error(`invalid deployment config:\n- ${settingsProblems.join('\n- ')}`);
  const paths = [...subdirs(opts.agentsDir).map((d) => join(d, 'agent.json')), ...subdirs(opts.dataDir).map((d) => join(d, 'agent-def', 'agent.json'))].filter(existsSync);
  const result: Validation = { loaded: 0, failures: [] };
  for (const name of deploymentEnv(deploymentExtensions)) process.env[name] ??= 'placeholder';
  for (const path of paths) {
    try {
      // Factories throw on unset service env; a placeholder lets them build without contacting anything.
      const extensions = JSON.parse(readFileSync(path, 'utf8')).extensions ?? {};
      for (const name of declaredEnv(extensions, catalog)) process.env[name] ??= 'placeholder';
      await loadAgent(path, deployment, catalog, deploymentExtensions);
      result.loaded++;
    } catch (err) {
      result.failures.push(`${path}: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
  return result;
}

if (import.meta.main) {
  const env = (name: string) => process.env[name] ?? (console.error(`${name} is required`), process.exit(2));
  const { loaded, failures } = await validate({ config: env('QUACK_CONFIG'), agentsDir: env('QUACK_AGENTS_DIR'), dataDir: env('QUACK_DATA_DIR') });
  for (const f of failures) console.log(`FAIL ${f.replaceAll('\n', ' ')}`);
  console.log(`${loaded} loaded, ${failures.length} failed`);
  process.exit(failures.length ? 1 : 0);
}
