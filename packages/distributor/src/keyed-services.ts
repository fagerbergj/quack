import type { ProxiedService } from '@quack/sdk';
import { manifests } from '@quack/runtime/catalog';
import { providerEnv, type DeploymentConfig } from '@quack/session';
import type { CredentialBroker } from './credentials.ts';

type Proxy = { broker: CredentialBroker; distributorUrl: string; token: string };
const route = (p: Proxy, name: string) => `${p.distributorUrl}/proxy/${name}`;
// Two manifests naming the same proxied service would both map to one /proxy/<name> route; refuse the deployment.
export const assertServiceNames = (services: readonly ProxiedService[]) => {
  const dup = services.find((s, i) => services.findIndex((t) => t.name === s.name) !== i);
  if (dup) throw new Error(`proxied service ${dup.name} is declared by two extensions`);
  return services;
};
export const defaultServices = assertServiceNames(manifests.flatMap((m) => m.proxiedServices ?? []));

/** Services that take an API key: the session gets the broker route and its launch token in the slots its client reads; the real key is added server-side. */
export function proxyKeyedServices(inherited: NodeJS.ProcessEnv, p: Proxy, services: readonly ProxiedService[] = defaultServices): Record<string, string> {
  const env: Record<string, string> = {};
  for (const s of services) {
    const url = inherited[s.urlEnv] ?? s.defaultUrl;
    const secret = inherited[s.secretEnv];
    if (!url || !secret) continue;
    if ('auth' in s) {
      const user = inherited[s.userEnv];
      if (!user) continue;
      p.broker.register(s.name, url, { authorization: `Basic ${Buffer.from(`${user}:${secret}`).toString('base64')}` });
      // The public key is not secret, but the real one stays server-side with its pair.
      Object.assign(env, { [s.urlEnv]: route(p, s.name), [s.userEnv]: s.userPlaceholder, [s.secretEnv]: p.token });
    } else {
      p.broker.register(s.name, url, { [s.header]: secret });
      Object.assign(env, { [s.urlEnv]: route(p, s.name), [s.secretEnv]: p.token });
    }
  }
  return env;
}

/** The deployment config with every provider pointed at a broker route, since a session container cannot reach the LAN; the key variable carries the launch token, and the real key (if any) is added server-side. */
export function proxyProviders(config: { providers?: Record<string, { baseUrl: string; apiKeyEnv?: string }> }, inherited: NodeJS.ProcessEnv, p: Proxy) {
  const env: Record<string, string> = {};
  const taken = new Set<string>();
  const providers = Object.fromEntries(Object.entries(config.providers ?? {}).map(([id, provider]) => {
    const keyVar = providerEnv({ providers: { [id]: provider } } as DeploymentConfig)[0];
    const key = inherited[keyVar];
    // Ids like a.b and a_b sanitize alike; each still needs its own route and key.
    const base = `PROVIDER_${id.replace(/\W/g, '_')}`;
    let name = base;
    for (let i = 2; taken.has(name); i++) name = `${base}_${i}`;
    taken.add(name);
    p.broker.register(name, provider.baseUrl, key || {});
    env[keyVar] = p.token;
    return [id, { ...provider, baseUrl: route(p, name) }];
  }));
  return { config: { ...config, providers }, env };
}
