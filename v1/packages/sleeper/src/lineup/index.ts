import { createSleeperClient } from '../client.ts';
import { lineupWorkflow, LINEUP_WORKFLOW } from './workflow.ts';

export { buildCard, cardsKind, type LineupCard } from './card.ts';
export { buildFacts, type Facts } from './facts.ts';
export { LINEUP_AGENT, lineupKey, lineupRuns, registerLineupSchedules, startLineup } from './jobs.ts';
export { lineupEvidence, lineupRubric } from './rubric.ts';
export { lineupWorkflow, LINEUP_WORKFLOW, type LineupDeps } from './workflow.ts';

/** The lineup workflow wired to the Sleeper API and the service half named in the environment (QUACK_SLEEPER_URL, QUACK_SLEEPER_TOKEN). */
export function lineupFromEnv(env: Record<string, string | undefined> = process.env) {
  const need = (name: string) => {
    const value = env[name];
    if (!value) throw new Error(`${name} is required by the ${LINEUP_WORKFLOW} workflow`);
    return value;
  };
  const [url, token] = [need('QUACK_SLEEPER_URL'), need('QUACK_SLEEPER_TOKEN')];
  const snapshot = async (league: string) => {
    const res = await fetch(`${url}/leagues/${encodeURIComponent(league)}/snapshots`, { method: 'POST', headers: { authorization: `Bearer ${token}` } });
    if (!res.ok) throw new Error(`sleeper service: snapshot for ${league} returned ${res.status}`);
    return res.json();
  };
  return lineupWorkflow({ client: createSleeperClient(), snapshot });
}
