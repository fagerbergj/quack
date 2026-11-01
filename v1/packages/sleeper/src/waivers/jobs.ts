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

export type WaiverSettings = { waiver_day_of_week?: number; daily_waivers?: number; daily_waivers_hour?: number; daily_waivers_days?: number | string };

const PT = 'America/Los_Angeles';
/** Sleeper counts weekdays from Monday = 0. */
const mondayIndex = (weekday: string) => (WEEKDAYS.indexOf(weekday) + 6) % 7;

const validMask = (v: unknown) => typeof v === 'string' && /^[01]{7}$/.test(v);

// Weekly waivers run at 00:00 Pacific on `waiver_day_of_week` (verified on a real league). Daily mode needs `daily_waivers_days`
// as a 7-char 0/1 string, Monday first; the real league sends an int of unknown encoding, so anything else fails closed.
function processingSlots(s: WaiverSettings): { days: number[]; hour: number } | null {
  if (s.daily_waivers === 1) {
    if (!validMask(s.daily_waivers_days)) return null;
    const days = (s.daily_waivers_days as string).split('').flatMap((c, i) => (c === '1' ? [i] : []));
    return days.length ? { days, hour: s.daily_waivers_hour ?? 0 } : null;
  }
  return typeof s.waiver_day_of_week === 'number' ? { days: [s.waiver_day_of_week], hour: 0 } : null;
}

/** The next processing instant strictly after `now`; null when the league sets none. */
export function nextProcessing(now: number, settings: WaiverSettings): number | null {
  const slots = processingSlots(settings);
  if (!slots) return null;
  const today = Date.parse(`${etDate(now, PT)}T12:00:00Z`);
  for (let i = 0; i < 9; i++) {
    const date = etDate(today + i * 86_400_000, PT);
    const at = etWall(date, slots.hour, PT);
    if (at > now && slots.days.includes(mondayIndex(etParts(at, PT).weekday))) return at;
  }
  return null;
}

/** One run before the league's next waiver processing, due an hour ahead of it (SL-9); null when there is none to serve. */
export function waiversRun(now: number, settings: WaiverSettings | undefined) {
  const processing = settings && nextProcessing(now, settings);
  if (!processing) return null;
  const deadline = processing - MARGIN_MS;
  // A late registration still fires, before the deadline.
  return deadline > now ? { at: Math.max(processing - LEAD_MS, now), deadline, processing } : null;
}

const inputOf = (i: LineupInput) => JSON.stringify(i);

/** Registers the next waiver run with the schedules service; re-registering an id replaces it, so it is safe to repeat. */
export async function registerWaiversSchedules(config: ScheduleConfig, client: SleeperClient, league: string, owner: string, now: () => number = Date.now): Promise<string[]> {
  const state = await client.state();
  if (state.season_type !== 'regular') return [];
  const settings = (await client.league(league)).settings;
  const run = waiversRun(now(), settings);
  if (settings?.daily_waivers === 1 && !validMask(settings.daily_waivers_days)) console.error(`sleeper: league ${league} has daily waivers with daily_waivers_days ${JSON.stringify(settings.daily_waivers_days)}, which is not understood; no waivers run is scheduled`);
  if (!run) return [];
  // Keyed by the processing date, not the week, so a re-registration after the NFL week turns replaces the entry rather than adding a stale one.
  const id = `sleeper/waivers/${league}/${etDate(run.processing, PT)}`;
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
