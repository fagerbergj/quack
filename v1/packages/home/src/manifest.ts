import type { Action, ExtensionManifest } from '@quack/sdk';
import { homeSessionExtensions } from './index.ts';

const envOf = (actions: Action<any>[]) => [...new Set([...actions.flatMap((a) => a.env ?? []), 'QUACK_INDEX_URL', 'QUACK_INDEX_TOKEN'])];

/** A fetch that presents the session's launch credential, which the distributor's session listener requires; a caller's own credential is kept. */
export function withCredential(token: string, doFetch: typeof fetch = fetch): typeof fetch {
  return (input, init) => {
    const headers = new Headers(init?.headers);
    if (!headers.has('authorization')) headers.set('authorization', `Bearer ${token}`);
    return doFetch(input, { ...init, headers });
  };
}

/** The home agent offers every action the image's extensions declare. A session's distributor is the host that spawned it (QUACK_INDEX_URL), reached with its launch credential. */
export const manifest: ExtensionManifest = {
  name: 'home',
  env: (_config, _workflows, actions) => envOf(actions),
  session: (_config, _workflows, actions) => {
    const env = Object.fromEntries(envOf(actions).map((n) => [n, process.env[n]]));
    if (!env.QUACK_INDEX_URL || !env.QUACK_INDEX_TOKEN) throw new Error('QUACK_INDEX_URL and QUACK_INDEX_TOKEN are required by the home extension');
    return homeSessionExtensions(actions, { distributorUrl: env.QUACK_INDEX_URL, fetch: withCredential(env.QUACK_INDEX_TOKEN), env });
  },
};
