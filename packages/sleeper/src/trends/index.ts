import { appEndpoint } from '@quack/sdk';
import { appGet, depsFromEnv } from '../deps.ts';
import { trendsWorkflow, TRENDS_WORKFLOW, type TrendsDeps } from './workflow.ts';

export { buildTrendsCard, type TrendsCard } from './card.ts';
export { buildTrendsFacts, diffSnapshots, type TrendsFacts } from './facts.ts';
export { registerTrendsSchedules, startTrends, trendsKey, TRENDS_AGENT } from './jobs.ts';
export { trendsEvidence, trendsRubric } from './rubric.ts';
export { trendsWorkflow, TRENDS_WORKFLOW, type TrendsDeps } from './workflow.ts';

/** The trends workflow wired to the Sleeper API and the service half named in the environment. */
export function trendsFromEnv(env: Record<string, string | undefined> = process.env) {
  const { url, token } = appEndpoint('sleeper');
  const get = appGet();
  const path = (league: string) => `/leagues/${encodeURIComponent(league)}/snapshots`;
  const deps: TrendsDeps = {
    ...depsFromEnv(env),
    snapshot: async (league) => {
      const res = await fetch(`${url}${path(league)}`, { method: 'POST', headers: { authorization: `Bearer ${token}` } });
      if (!res.ok) throw new Error(`sleeper service: snapshot for ${league} returned ${res.status}`);
      return res.json();
    },
    history: async (league) => (await get(path(league))).snapshots,
    snapshotOn: (league, date) => get(`${path(league)}/${date}`),
  };
  return trendsWorkflow(deps);
}
