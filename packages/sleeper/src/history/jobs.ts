import { startJob, type StartConfig } from '../jobs.ts';
import type { HistoryInput } from './facts.ts';

export const HISTORY_AGENT = 'sleeper-history';

/** Reruns within a week continue one session. A season review has no deadline, so it is never scheduled. */
export const historyKey = (league: string, week: number) => `sleeper:${league}:${week}:history`;

/** On-demand start: each call is its own submission under the week's session key. */
export const startHistory = (config: StartConfig, input: HistoryInput) => startJob(config, HISTORY_AGENT, historyKey(input.league, input.week), input);
