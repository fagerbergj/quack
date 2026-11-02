import { json } from '../../api.ts';

// The page never holds the Sleeper service token: the distributor adds it (apps/sleeper/*).
export const sleeperLeague = async () => (await json<{ league: string }>(await fetch('apps/sleeper'))).league;
// The week is the service's current one; a past week is never re-run from the page.
export const runJob = async (job: string) => json<{ sessionKey: string }>(await fetch(`apps/sleeper/${job}`, { method: 'POST' }));
