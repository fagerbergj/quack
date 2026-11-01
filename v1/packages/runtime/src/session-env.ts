import { readFileSync } from 'node:fs';
import { declaredEnv, parseDeployment, providerEnv, shellEnv } from '@quack/session';
import { catalog } from './catalog.ts';

/** What a session process inherits from the host: toolchain basics, its model keys, and what its agent's extensions declare. */
export function sessionEnv(agentPath: string | undefined, source: NodeJS.ProcessEnv = process.env): NodeJS.ProcessEnv {
  const config = source.QUACK_CONFIG ? parseDeployment(JSON.parse(readFileSync(source.QUACK_CONFIG, 'utf8'))) : undefined;
  const extensions = agentPath ? (JSON.parse(readFileSync(agentPath, 'utf8')).extensions ?? {}) : {};
  return { ...shellEnv(source, [...(config ? providerEnv(config) : []), ...declaredEnv(extensions, catalog)]), ...(source.QUACK_CONFIG && { QUACK_CONFIG: source.QUACK_CONFIG }) };
}
