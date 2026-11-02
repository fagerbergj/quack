import type { Action, ExtensionManifest } from '@quack/sdk';
import { homeSessionExtensions } from './index.ts';

const envOf = (actions: Action<any>[]) => [...new Set([...actions.flatMap((a) => a.env ?? []), 'QUACK_INDEX_URL'])];

/** The home agent offers every action the image's extensions declare. A session's distributor is the host that spawned it (QUACK_INDEX_URL). */
export const manifest: ExtensionManifest = {
  name: 'home',
  env: (_config, _workflows, actions) => envOf(actions),
  session: (_config, _workflows, actions) => {
    const env = Object.fromEntries(envOf(actions).map((n) => [n, process.env[n]]));
    if (!env.QUACK_INDEX_URL) throw new Error('QUACK_INDEX_URL is required by the home extension');
    return homeSessionExtensions(actions, { distributorUrl: env.QUACK_INDEX_URL, fetch, env });
  },
};
