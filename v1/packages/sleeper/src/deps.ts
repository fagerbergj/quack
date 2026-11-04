import { appEndpoint } from '@quack/sdk';
import { createSleeperClient } from './client.ts';
import { exaNews } from './lineup/news.ts';
import type { LineupDeps } from './lineup/workflow.ts';

/** A job's wiring from the environment: the Sleeper API, the Sleeper app (QUACK_INDEX_URL, QUACK_INDEX_TOKEN) and, when QUACK_EXA_API_KEY is set, news. */
export function depsFromEnv(env: Record<string, string | undefined> = process.env): LineupDeps {
  const { url, token } = appEndpoint('sleeper', env);
  const snapshot = async (league: string) => {
    const res = await fetch(`${url}/leagues/${encodeURIComponent(league)}/snapshots`, { method: 'POST', headers: { authorization: `Bearer ${token}` } });
    if (!res.ok) throw new Error(`sleeper service: snapshot for ${league} returned ${res.status}`);
    return res.json();
  };
  // Without the Exa key the check is simply not scored, and the verdict says so.
  const apiKey = env.QUACK_EXA_API_KEY;
  return { client: createSleeperClient(), snapshot, news: apiKey ? exaNews({ apiKey, url: env.QUACK_EXA_URL }) : undefined };
}
