import type { SleeperClient } from '../client.ts';
import { startJob, type StartConfig } from '../jobs.ts';
import { etDate, etParts, etWall, MARGIN_MS, WEEKDAYS } from '../lineup/jobs.ts';
import type { LineupInput } from '../lineup/facts.ts';
import { registerSchedule, type ScheduleConfig } from '../schedules.ts';

export const WAIVERS_AGENT = 'sleeper-waivers';
/** How long before processing the scheduled run starts, so the owner can still act on it. */
const LEAD_MS = 12 * 60 * 60_000;

/** Reruns within a week continue one session. */
export const waiversKey = (league: string, week: number) => `sleeper:${league}:${week}:waivers`;

export type WaiverSettings = { waiver_day_of_week?: number; daily_waivers?: number };

const PT = 'America/Los_Angeles';
/** Sleeper counts weekdays from Monday = 0. */
const mondayIndex = (weekday: string) => (WEEKDAYS.indexOf(weekday) + 6) % 7;

// Weekly waivers run at 00:00 Pacific on `waiver_day_of_week` (verified on a real league). Daily waivers send their days in
// an encoding nobody has decoded, so a daily league gets no run rather than a guessed one.
const processingDay = (s: WaiverSettings) => (s.daily_waivers !== 1 && typeof s.waiver_day_of_week === 'number' ? s.waiver_day_of_week : null);

/** The next processing instant strictly after `now`; null when the league sets none. */
export function nextProcessing(now: number, settings: WaiverSettings): number | null {
  const day = processingDay(settings);
  if (day === null) return null;
  const today = Date.parse(`${etDate(now, PT)}T12:00:00Z`);
  for (let i = 0; i < 9; i++) {
    const at = etWall(etDate(today + i * 86_400_000, PT), 0, PT);
    if (at > now && mondayIndex(etParts(at, PT).weekday) === day) return at;
  }
  return null;
}

/** One run before the league's next waiver processing, due an hour ahead of it; null when there is none to serve. */
export function waiversRun(now: number, settings: WaiverSettings | undefined) {
  const processing = settings && nextProcessing(now, settings);
  if (!processing) return null;
  // A fixed time: the schedules service fires one registered late, before its deadline, and once only.
  return { at: processing - LEAD_MS, deadline: processing - MARGIN_MS, processing };
}

/** Registers the next waiver run with the schedules service; re-registering an id replaces it, so it is safe to repeat. */
export async function registerWaiversSchedules(config: ScheduleConfig, client: SleeperClient, league: string, owner: string, now: () => number = Date.now): Promise<string[]> {
  const state = await client.state();
  if (state.season_type !== 'regular') return [];
  const settings = (await client.league(league)).settings;
  const run = waiversRun(now(), settings);
  if (settings?.daily_waivers === 1) console.error(`sleeper: league ${league} has daily waivers, which are not understood; no waivers run is scheduled`);
  if (!run) return [];
  // One pending run per league: re-registering replaces its time and input, even after the processing day or the NFL week changes.
  const input: LineupInput = { league, week: state.week, owner };
  return [await registerSchedule(config, { id: `sleeper/waivers/${league}`, agent: WAIVERS_AGENT, at: run.at, deadline: run.deadline, sessionKey: waiversKey(league, state.week), input })];
}

/** On-demand start: the "Run now" path; each call is its own submission under the week's session key. */
export const startWaivers = (config: StartConfig, input: LineupInput) => startJob(config, WAIVERS_AGENT, waiversKey(input.league, input.week), input);
