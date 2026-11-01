import type { SleeperClient } from '../client.ts';
import { startJob, type StartConfig } from '../jobs.ts';
import { MARGIN_MS } from '../lineup/jobs.ts';
import type { LineupInput } from '../lineup/facts.ts';
import type { ScheduleConfig } from '../schedules.ts';

export const DIGEST_AGENT = 'sleeper-digest';
export const RETRO_AGENT = 'sleeper-retro';
/** The preview runs this long before the week's first kickoff, the recap this long after its last (games run about three and a half hours). */
const PREVIEW_LEAD_MS = 6 * 60 * 60_000;
const RECAP_LAG_MS = 5 * 60 * 60_000;
/** A recap stays useful until the next week's preview is due. */
const RECAP_STALE_MS = 3 * 24 * 60 * 60_000;

/** Reruns within a week continue one session per job. */
export const digestKey = (league: string, week: number) => `sleeper:${league}:${week}:digest`;
export const retroKey = (league: string, week: number) => `sleeper:${league}:${week}:retro`;

/** Preview before the first kickoff (due an hour ahead, SL-9), recap and retro once the last game is final (audit schedule rows). */
export function reportRuns(kickoffs: number[], league: string, week: number) {
  if (!kickoffs.length) return [];
  const [first, last] = [Math.min(...kickoffs), Math.max(...kickoffs)];
  const recap = { at: last + RECAP_LAG_MS, deadline: last + RECAP_LAG_MS + RECAP_STALE_MS };
  const id = (job: string) => `sleeper/${job}/${league}/${week}`;
  return [
    { id: `${id('digest')}/preview`, agent: DIGEST_AGENT, sessionKey: digestKey(league, week), at: first - PREVIEW_LEAD_MS, deadline: first - MARGIN_MS },
    { id: `${id('digest')}/recap`, agent: DIGEST_AGENT, sessionKey: digestKey(league, week), ...recap },
    { id: id('retro'), agent: RETRO_AGENT, sessionKey: retroKey(league, week), ...recap },
  ];
}

/** Registers this week's digest and retro runs with the schedules service; re-registering an id replaces it, so it is safe to repeat. */
export async function registerReportSchedules(config: ScheduleConfig, client: SleeperClient, league: string, owner: string): Promise<string[]> {
  const state = await client.state();
  if (state.season_type !== 'regular') return [];
  const scores = await client.scores(state.season, state.week);
  const starts = scores.filter((s: any) => s.start_time > 0 && !['flex-schedule', 'postponed', 'cancelled'].includes(s.metadata?.status)).map((s: any) => s.start_time as number);
  const input = JSON.stringify({ league, week: state.week, owner } satisfies LineupInput);
  const ids: string[] = [];
  for (const run of reportRuns(starts, league, state.week)) {
    const res = await (config.fetch ?? fetch)(`${config.url}/schedules`, {
      method: 'POST',
      headers: { authorization: `Bearer ${config.token}` },
      body: JSON.stringify({ id: run.id, agent: run.agent, times: [new Date(run.at).toISOString()], deadline: new Date(run.deadline).toISOString(), sessionKey: run.sessionKey, input }),
    });
    if (!res.ok) throw new Error(`schedules: registering ${run.id} returned ${res.status}`);
    ids.push(run.id);
  }
  return ids;
}

/** On-demand start (SL-2): each call is its own submission under the week's session key. */
export const startDigest = (config: StartConfig, input: LineupInput) => startJob(config, DIGEST_AGENT, digestKey(input.league, input.week), input);
export const startRetro = (config: StartConfig, input: LineupInput) => startJob(config, RETRO_AGENT, retroKey(input.league, input.week), input);
