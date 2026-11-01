import { startJob, type StartConfig } from '../jobs.ts';
import type { HistoryInput } from './facts.ts';

export const HISTORY_AGENT = 'sleeper-history';

/** Reruns within a week continue one session. A season review has no deadline, so it is never scheduled. */
export const historyKey = (league: string, week: number) => `sleeper:${league}:${week}:history`;

/** On-demand start (SL-2): each call is its own submission under the week's session key. */
export const startHistory = (config: StartConfig, input: HistoryInput) => startJob(config, HISTORY_AGENT, historyKey(input.league, input.week), input);

/** `?seasons=3` caps how many seasons back are reviewed; absent means every reachable one. */
export function historyParams(params: URLSearchParams): { seasons?: number } | string {
  const raw = params.get('seasons');
  if (raw === null) return {};
  const n = Number(raw);
  return Number.isInteger(n) && n >= 1 ? { seasons: n } : 'bad seasons';
}
