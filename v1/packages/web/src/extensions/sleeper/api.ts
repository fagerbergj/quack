import { json } from '../../api.ts';

// The page never holds the Sleeper service token: the distributor adds it (apps/sleeper/*).
export const sleeperLeague = async () => (await json<{ league: string }>(await fetch('apps/sleeper'))).league;
// The week is the service's current one; a past week is never re-run from the page.
export const runJob = async (job: string) => json<{ sessionKey: string }>(await fetch(`apps/sleeper/${job}`, { method: 'POST' }));
// The live week and upcoming runs are decoration over the sessions: without them the page falls back to the newest session's week and drops the times.
export type Run = { job: string; at: string };
export type Live = { week?: number; runs: Run[] };
export const upcoming = async (): Promise<Live> => fetch('apps/sleeper/schedule').then((r) => (r.ok ? r.json() : { runs: [] }), () => ({ runs: [] }));
