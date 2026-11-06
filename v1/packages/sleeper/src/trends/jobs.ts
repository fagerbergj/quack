import type { SleeperClient } from '../client.ts';
import { startJob, type StartConfig } from '../jobs.ts';
import { ET } from '../lineup/jobs.ts';
import type { LineupInput } from '../lineup/facts.ts';
import { registerSchedule, removeSchedule, type ScheduleConfig } from '../schedules.ts';

export const TRENDS_AGENT = 'sleeper-trends';
// The facts step takes today's snapshot itself; the default window runs to the next occurrence.
const CRON = '30 4 * * *';

/** Reruns within a week continue one session. */
export const trendsKey = (league: string, week: number) => `sleeper:${league}:${week}:trends`;
const scheduleId = (league: string) => `sleeper/trends/${league}`;

/** Registers the daily run for the current week; re-registering the id replaces it (carrying the new week's key), and outside the regular season it is removed. */
export async function registerTrendsSchedules(config: ScheduleConfig, client: SleeperClient, league: string, owner: string): Promise<string[]> {
  const state = await client.state();
  const id = scheduleId(league);
  if (state.season_type !== 'regular') {
    await removeSchedule(config, id);
    return [];
  }
  const input: LineupInput = { league, week: state.week, owner };
  return [await registerSchedule(config, { id, agent: TRENDS_AGENT, cron: CRON, tz: ET, sessionKey: trendsKey(league, state.week), input })];
}

/** On-demand start: each call is its own submission under the week's session key. */
export const startTrends = (config: StartConfig, input: LineupInput) => startJob(config, TRENDS_AGENT, trendsKey(input.league, input.week), input);
