import { json } from '../api.ts';

export type PrState = { state: 'open' | 'merged' | 'closed'; at: string };
export type Pulls = { [key: string]: PrState };

// The github service answers at most this many keys per lookup and flags the rest as truncated.
const BATCH = 100;

/** PR states for github session keys, looked up in batches the service accepts; undefined when the service is not connected. */
export async function lookupPulls(keys: string[]): Promise<Pulls | undefined> {
  const batches = Array.from({ length: Math.ceil(keys.length / BATCH) }, (_, i) => keys.slice(i * BATCH, (i + 1) * BATCH));
  const pages = await Promise.all(batches.map(async (batch) => {
    const res = await fetch(`apps/github/pulls?keys=${encodeURIComponent(batch.join(','))}`);
    return res.status === 404 ? undefined : json<{ pulls: Pulls; truncated?: boolean }>(res);
  }));
  if (pages.some((p) => p === undefined)) return undefined;
  if (pages.some((p) => p!.truncated)) throw new Error('the GitHub service answered only part of the lookup');
  return Object.assign({}, ...pages.map((p) => p!.pulls));
}
