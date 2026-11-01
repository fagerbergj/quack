import type { SleeperClient } from '../client.ts';
import { startJob, type StartConfig } from '../jobs.ts';
import { etDate, etParts, etWall, MARGIN_MS, WEEKDAYS } from '../lineup/jobs.ts';
import type { LineupInput } from '../lineup/facts.ts';
import type { ScheduleConfig } from '../schedules.ts';

export const WAIVERS_AGENT = 'sleeper-waivers';
/** How long before processing the scheduled run starts, so the owner can still act on it. */
const LEAD_MS = 12 * 60 * 60_000;

/** Reruns within a week continue one session. */
export const waiversKey = (league: string, week: number) => `sleeper:${league}:${week}:waivers`;

// Assumes Sleeper's `waiver_day_of_week` counts from Sunday (0) and `daily_waivers_hour` is New York wall time; verify on the real league.
/** The next processing instant strictly after `now`, from the league's weekday and hour. */
export function nextProcessing(now: number, day: number, hour: number): number {
  const today = Date.parse(`${etDate(now)}T12:00:00Z`);
  for (let i = 0; i < 8; i++) {
    const at = etWall(etDate(today + i * 86_400_000), hour);
    if (at > now && WEEKDAYS.indexOf(etParts(at).weekday) === day) return at;
  }
  throw new Error(`sleeper: no processing time for day ${day} at ${hour}`);
}

/** One run before the league's next waiver processing, due an hour ahead of it (SL-9); null when the league sets no processing day. */
export function waiversRun(now: number, settings: { waiver_day_of_week?: number; daily_waivers_hour?: number } | undefined) {
  if (typeof settings?.waiver_day_of_week !== 'number') return null;
  const processing = nextProcessing(now, settings.waiver_day_of_week, settings.daily_waivers_hour ?? 0);
  const deadline = processing - MARGIN_MS;
  return deadline > now ? { at: processing - LEAD_MS, deadline, processing } : null;
}

const inputOf = (i: LineupInput) => JSON.stringify(i);

/** Registers the next waiver run with the schedules service; re-registering an id replaces it, so it is safe to repeat. */
export async function registerWaiversSchedules(config: ScheduleConfig, client: SleeperClient, league: string, owner: string, now: () => number = Date.now): Promise<string[]> {
  const state = await client.state();
  if (state.season_type !== 'regular') return [];
  const run = waiversRun(now(), (await client.league(league)).settings);
  if (!run) return [];
  const id = `sleeper/waivers/${league}/${state.week}`;
  const res = await (config.fetch ?? fetch)(`${config.url}/schedules`, {
    method: 'POST',
    headers: { authorization: `Bearer ${config.token}` },
    body: JSON.stringify({ id, agent: WAIVERS_AGENT, times: [new Date(run.at).toISOString()], deadline: new Date(run.deadline).toISOString(), sessionKey: waiversKey(league, state.week), input: inputOf({ league, week: state.week, owner }) }),
  });
  if (!res.ok) throw new Error(`schedules: registering ${id} returned ${res.status}`);
  return [id];
}

/** On-demand start (SL-2): the "Run now" path; each call is its own submission under the week's session key. */
export const startWaivers = (config: StartConfig, input: LineupInput) => startJob(config, WAIVERS_AGENT, waiversKey(input.league, input.week), input);
