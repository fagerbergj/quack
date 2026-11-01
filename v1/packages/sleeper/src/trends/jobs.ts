import type { SleeperClient } from '../client.ts';
import { startJob, type StartConfig } from '../jobs.ts';
import { ET } from '../lineup/jobs.ts';
import type { LineupInput } from '../lineup/facts.ts';
import type { ScheduleConfig } from '../schedules.ts';

export const TRENDS_AGENT = 'sleeper-trends';
// Half an hour after the 04:00 ET snapshot (schedules.ts), so the run diffs today's snapshot; the default window runs to the next occurrence.
const CRON = '30 4 * * *';

/** Reruns within a week continue one session. */
export const trendsKey = (league: string, week: number) => `sleeper:${league}:${week}:trends`;
const scheduleId = (league: string) => `sleeper/trends/${league}`;

/** Registers the daily run for the current week; re-registering the id replaces it (carrying the new week's key), and outside the regular season it is removed. */
export async function registerTrendsSchedules(config: ScheduleConfig, client: SleeperClient, league: string, owner: string): Promise<string[]> {
  const state = await client.state();
  const id = scheduleId(league);
  const call = config.fetch ?? fetch;
  const headers = { authorization: `Bearer ${config.token}` };
  if (state.season_type !== 'regular') {
    const res = await call(`${config.url}/schedules/${encodeURIComponent(id)}`, { method: 'DELETE', headers });
    if (!res.ok && res.status !== 404) throw new Error(`schedules: removing ${id} returned ${res.status}`);
    return [];
  }
  const input: LineupInput = { league, week: state.week, owner };
  const res = await call(`${config.url}/schedules`, { method: 'POST', headers, body: JSON.stringify({ id, agent: TRENDS_AGENT, cron: CRON, tz: ET, sessionKey: trendsKey(league, state.week), input: JSON.stringify(input) }) });
  if (!res.ok) throw new Error(`schedules: registering ${id} returned ${res.status}`);
  return [id];
}

/** On-demand start (SL-2): each call is its own submission under the week's session key. */
export const startTrends = (config: StartConfig, input: LineupInput) => startJob(config, TRENDS_AGENT, trendsKey(input.league, input.week), input);
