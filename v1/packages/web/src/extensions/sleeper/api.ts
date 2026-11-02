import { json } from '../../api.ts';

// The page never holds the Sleeper service token: the distributor adds it (apps/sleeper/*).
export const sleeperLeague = async () => (await json<{ league: string }>(await fetch('apps/sleeper'))).league;
// The week is the service's current one; a past week is never re-run from the page.
export const runJob = async (job: string) => json<{ sessionKey: string }>(await fetch(`apps/sleeper/${job}`, { method: 'POST' }));
// Upcoming runs are decoration: without them the strip and the sheet simply drop their times.
export type Run = { job: string; at: string };
export const upcoming = async (): Promise<Run[]> => (await fetch('apps/sleeper/schedule').then((r) => (r.ok ? r.json() : { runs: [] }), () => ({ runs: [] }))).runs;
