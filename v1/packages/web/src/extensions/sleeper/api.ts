import { json } from '../../api.ts';

// The page never holds the Sleeper service token: the distributor adds it (apps/sleeper/*).
export const sleeperLeague = async () => (await json<{ league: string }>(await fetch('apps/sleeper'))).league;
export const runLineup = async () => json<{ sessionKey: string }>(await fetch('apps/sleeper/lineup', { method: 'POST' }));
