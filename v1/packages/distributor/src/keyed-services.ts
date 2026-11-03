import { providerEnv } from '@quack/session';
import type { CredentialBroker } from './credentials.ts';

type Proxy = { broker: CredentialBroker; distributorUrl: string; token: string };
const route = (p: Proxy, name: string) => `${p.distributorUrl}/svc/${name}`;

/** Services that take an API key: the session gets the broker route and its launch token in the slots its client reads; the real key is added server-side. */
export function proxyKeyedServices(inherited: NodeJS.ProcessEnv, p: Proxy): Record<string, string> {
  const env: Record<string, string> = {};
  if (inherited.QUACK_EXA_API_KEY) {
    p.broker.register('QUACK_EXA', inherited.QUACK_EXA_URL ?? 'https://api.exa.ai', { 'x-api-key': inherited.QUACK_EXA_API_KEY });
    Object.assign(env, { QUACK_EXA_URL: route(p, 'QUACK_EXA'), QUACK_EXA_API_KEY: p.token });
  }
  const { LANGFUSE_BASE_URL: base, LANGFUSE_PUBLIC_KEY: pub, LANGFUSE_SECRET_KEY: secret } = inherited;
  if (base && pub && secret) {
    p.broker.register('LANGFUSE', base, { authorization: `Basic ${Buffer.from(`${pub}:${secret}`).toString('base64')}` });
    // The public key is not secret, but the real one stays server-side with its pair.
    Object.assign(env, { LANGFUSE_BASE_URL: route(p, 'LANGFUSE'), LANGFUSE_PUBLIC_KEY: 'session', LANGFUSE_SECRET_KEY: p.token });
  }
  return env;
}

/** The deployment config with each keyed provider pointed at a broker route (its key variable carries the launch token); keyless providers stay as they are. */
export function proxyProviders(config: { providers?: Record<string, { baseUrl: string; apiKeyEnv?: string }> }, inherited: NodeJS.ProcessEnv, p: Proxy) {
  const env: Record<string, string> = {};
  const providers = Object.fromEntries(Object.entries(config.providers ?? {}).map(([id, provider]) => {
    const keyVar = providerEnv({ providers: { [id]: provider } } as never)[0];
    const key = inherited[keyVar];
    if (!key) return [id, provider];
    const name = `PROVIDER_${id.replace(/\W/g, '_')}`;
    p.broker.register(name, provider.baseUrl, key);
    env[keyVar] = p.token;
    return [id, { ...provider, baseUrl: route(p, name) }];
  }));
  return { config: { ...config, providers }, env };
}
