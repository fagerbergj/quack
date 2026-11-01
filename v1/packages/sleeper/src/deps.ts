import { createSleeperClient } from './client.ts';
import { exaNews } from './lineup/news.ts';
import type { LineupDeps } from './lineup/workflow.ts';

/** A job's wiring from the environment: the Sleeper API, the service half (QUACK_SLEEPER_URL, QUACK_SLEEPER_TOKEN) and, when QUACK_EXA_API_KEY is set, news. */
export function depsFromEnv(workflow: string, env: Record<string, string | undefined> = process.env): LineupDeps {
  const need = (name: string) => {
    const value = env[name];
    if (!value) throw new Error(`${name} is required by the ${workflow} workflow`);
    return value;
  };
  const [url, token] = [need('QUACK_SLEEPER_URL'), need('QUACK_SLEEPER_TOKEN')];
  const snapshot = async (league: string) => {
    const res = await fetch(`${url}/leagues/${encodeURIComponent(league)}/snapshots`, { method: 'POST', headers: { authorization: `Bearer ${token}` } });
    if (!res.ok) throw new Error(`sleeper service: snapshot for ${league} returned ${res.status}`);
    return res.json();
  };
  // Without the Exa key the check is simply not scored, and the verdict says so.
  const apiKey = env.QUACK_EXA_API_KEY;
  return { client: createSleeperClient(), snapshot, news: apiKey ? exaNews({ apiKey }) : undefined };
}
