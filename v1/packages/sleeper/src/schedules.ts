export type ScheduleConfig = { url: string; token: string; fetch?: typeof fetch };

/** Registers the daily snapshot with the schedules service; re-registering the same id replaces it, so this is safe on every start. */
export async function registerSnapshotSchedule(config: ScheduleConfig, league: string) {
  const res = await (config.fetch ?? fetch)(`${config.url}/schedules`, {
    method: 'POST',
    headers: { authorization: `Bearer ${config.token}` },
    body: JSON.stringify({ id: `sleeper/snapshot/${league}`, cron: '0 4 * * *', tz: 'America/New_York', sessionKey: 'sleeper:snapshot:{time}', input: `snapshot ${league}` }),
  });
  if (!res.ok) throw new Error(`schedules: registering the snapshot job returned ${res.status}`);
}

/** Retries with doubling backoff (1s, cap 60s) until registered: compose starts the services together, so the schedules service is often not up yet. */
export async function registerUntilDone(config: ScheduleConfig, league: string, sleep: (ms: number) => Promise<unknown> = (ms) => new Promise((r) => setTimeout(r, ms))) {
  for (let delay = 1000; ; delay = Math.min(delay * 2, 60_000)) {
    try {
      return await registerSnapshotSchedule(config, league);
    } catch (err) {
      console.error(`${(err as Error).message}; retrying in ${delay}ms`);
      await sleep(delay);
    }
  }
}
