import { depsFromEnv } from '../deps.ts';
import { trendsWorkflow, TRENDS_WORKFLOW, type TrendsDeps } from './workflow.ts';

export { buildTrendsCard, type TrendsCard } from './card.ts';
export { buildTrendsFacts, diffSnapshots, type TrendsFacts } from './facts.ts';
export { registerTrendsSchedules, startTrends, trendsKey, TRENDS_AGENT } from './jobs.ts';
export { trendsEvidence, trendsRubric } from './rubric.ts';
export { trendsWorkflow, TRENDS_WORKFLOW, type TrendsDeps } from './workflow.ts';

/** The trends workflow wired to the Sleeper API and the service half named in the environment. */
export function trendsFromEnv(env: Record<string, string | undefined> = process.env) {
  const base = depsFromEnv(TRENDS_WORKFLOW, env);
  // Read now: the session process deletes these from process.env once its agent has loaded.
  const [url, token] = [env.QUACK_SLEEPER_URL, env.QUACK_SLEEPER_TOKEN];
  const get = async (path: string) => {
    const res = await fetch(`${url}${path}`, { headers: { authorization: `Bearer ${token}` } });
    if (!res.ok) throw new Error(`sleeper service: ${path} returned ${res.status}`);
    return res.json();
  };
  const deps: TrendsDeps = {
    ...base,
    history: async (league) => (await get(`/leagues/${encodeURIComponent(league)}/snapshots`)).snapshots,
    snapshotOn: (league, date) => get(`/leagues/${encodeURIComponent(league)}/snapshots/${date}`),
  };
  return trendsWorkflow(deps);
}
