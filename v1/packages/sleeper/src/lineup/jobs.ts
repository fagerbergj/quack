import type { SleeperClient } from '../client.ts';
import type { ScheduleConfig } from '../schedules.ts';
import type { LineupInput } from './facts.ts';

export const LINEUP_AGENT = 'sleeper-lineup';
/** SL-9: a scheduled lineup finishes this long before the kickoff it serves. */
const MARGIN_MS = 60 * 60_000;
const ET = 'America/New_York';

/** Reruns within a week continue one session. */
export const lineupKey = (league: string, week: number) => `sleeper:${league}:${week}:lineup`;

const etParts = (ms: number) => Object.fromEntries(new Intl.DateTimeFormat('en-US', { timeZone: ET, hourCycle: 'h23', weekday: 'short', year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' }).formatToParts(ms).map((p) => [p.type, p.value]));
const etDate = (ms: number) => new Intl.DateTimeFormat('en-CA', { timeZone: ET }).format(ms);

/** The instant of a New York wall-clock hour on a date, so a run stays at its local hour across DST. */
function etWall(date: string, hour: number): number {
  const guess = Date.parse(`${date}T${String(hour).padStart(2, '0')}:00:00Z`);
  const p = etParts(guess);
  return guess - (Date.UTC(+p.year, +p.month - 1, +p.day, +p.hour, +p.minute, +p.second) - guess);
}

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const wednesdayBefore = (ms: number) => {
  const back = (WEEKDAYS.indexOf(etParts(ms).weekday) - 3 + 7) % 7;
  return etDate(Date.parse(`${etDate(ms)}T12:00:00Z`) - back * 86_400_000);
};

/** After Wednesday's injury report, and again on Sunday morning; each is useful until an hour before the first kickoff it covers (SL-9, audit proposal). */
export function lineupRuns(kickoffs: number[]): Array<{ id: 'wed' | 'sun'; at: number; deadline: number }> {
  if (!kickoffs.length) return [];
  const first = Math.min(...kickoffs);
  const sunday = kickoffs.filter((k) => etParts(k).weekday === 'Sun');
  const runs: Array<{ id: 'wed' | 'sun'; at: number; deadline: number }> = [{ id: 'wed', at: etWall(wednesdayBefore(first), 17), deadline: first - MARGIN_MS }];
  if (sunday.length) runs.push({ id: 'sun', at: etWall(etDate(Math.min(...sunday)), 9), deadline: Math.min(...sunday) - MARGIN_MS });
  return runs.filter((r) => r.at < r.deadline);
}

const inputOf = (i: LineupInput) => JSON.stringify(i);

/** Registers this week's two lineup runs with the schedules service; re-registering an id replaces it, so it is safe to repeat. */
export async function registerLineupSchedules(config: ScheduleConfig, client: SleeperClient, league: string, owner: string): Promise<string[]> {
  const state = await client.state();
  if (state.season_type !== 'regular') return [];
  const scores = await client.scores(state.season, state.week);
  const kickoffs = scores.filter((s: any) => s.start_time > 0 && !['flex-schedule', 'postponed', 'cancelled'].includes(s.metadata?.status)).map((s: any) => s.start_time as number);
  const ids: string[] = [];
  for (const run of lineupRuns(kickoffs)) {
    const id = `sleeper/lineup/${league}/${state.week}/${run.id}`;
    const res = await (config.fetch ?? fetch)(`${config.url}/schedules`, {
      method: 'POST',
      headers: { authorization: `Bearer ${config.token}` },
      body: JSON.stringify({ id, agent: LINEUP_AGENT, times: [new Date(run.at).toISOString()], deadline: new Date(run.deadline).toISOString(), sessionKey: lineupKey(league, state.week), input: inputOf({ league, week: state.week, owner }) }),
    });
    if (!res.ok) throw new Error(`schedules: registering ${id} returned ${res.status}`);
    ids.push(id);
  }
  return ids;
}

/** On-demand start (SL-2): submits the job to the distributor under the week's session key; each call is its own submission. */
export async function startLineup(config: { distributorUrl: string; fetch?: typeof fetch; now: () => Date }, input: LineupInput) {
  const key = lineupKey(input.league, input.week);
  const url = `${config.distributorUrl}/sessions/${encodeURIComponent(key)}/invocations?agent=${LINEUP_AGENT}`;
  const res = await (config.fetch ?? fetch)(url, { method: 'POST', body: JSON.stringify({ input: inputOf(input), requestId: `start:${key}:${config.now().toISOString()}` }) });
  return { ok: res.ok, status: res.status, sessionKey: key };
}
