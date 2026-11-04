import { readFileSync } from 'node:fs';
import { declaredEnv, deploymentEnv, parseDeployment, providerEnv, shellEnv, type DeploymentConfig } from '@quack/session';
import { catalog, deploymentExtensions } from './catalog.ts';

/** `services.sleeper` reaches a session as QUACK_SLEEPER_URL and QUACK_SLEEPER_TOKEN, which only agents whose extensions declare those names receive. */
function serviceVars(config: DeploymentConfig | undefined, source: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  return Object.fromEntries(Object.entries(config?.services ?? {}).flatMap(([name, s]) => {
    const prefix = `QUACK_${name.toUpperCase().replace(/\W/g, '_')}`;
    const token = s.tokenEnv && source[s.tokenEnv];
    return [[`${prefix}_URL`, s.url], ...(token ? [[`${prefix}_TOKEN`, token]] : [])];
  }));
}

/** What a session process inherits from the host: toolchain basics, its model keys, and what its agent's extensions declare. */
export function sessionEnv(agentPath: string | undefined, source: NodeJS.ProcessEnv = process.env): NodeJS.ProcessEnv {
  const config = source.QUACK_CONFIG ? parseDeployment(JSON.parse(readFileSync(source.QUACK_CONFIG, 'utf8'))) : undefined;
  const extensions = agentPath ? (JSON.parse(readFileSync(agentPath, 'utf8')).extensions ?? {}) : {};
  const names = [...(config ? providerEnv(config) : []), ...declaredEnv(extensions, catalog), ...deploymentEnv(deploymentExtensions)];
  return { ...shellEnv({ ...source, ...serviceVars(config, source) }, names), ...(source.QUACK_CONFIG && { QUACK_CONFIG: source.QUACK_CONFIG }) };
}
