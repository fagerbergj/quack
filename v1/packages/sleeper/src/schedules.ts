/** `url` is a base URL that `fetch` resolves to the schedules app, whose routes are `/` and `/<id>`. */
export type ScheduleConfig = { url: string; fetch?: typeof fetch };

/** Deletes the retired daily snapshot schedule (trends takes its own snapshot); 404 means it is already gone. */
export async function removeLegacySnapshotSchedule(config: ScheduleConfig, league: string) {
  const res = await (config.fetch ?? fetch)(`${config.url}/${encodeURIComponent(`sleeper/snapshot/${league}`)}`, { method: 'DELETE' });
  if (!res.ok && res.status !== 404) throw new Error(`schedules: removing the legacy snapshot job returned ${res.status}`);
}

/** Retries with doubling backoff (1s, cap 60s) until removed: compose starts the services together, so the schedules service is often not up yet. */
export async function removeLegacyUntilDone(config: ScheduleConfig, league: string, sleep: (ms: number) => Promise<unknown> = (ms) => new Promise((r) => setTimeout(r, ms))) {
  for (let delay = 1000; ; delay = Math.min(delay * 2, 60_000)) {
    try {
      return await removeLegacySnapshotSchedule(config, league);
    } catch (err) {
      console.error(`${(err as Error).message}; retrying in ${delay}ms`);
      await sleep(delay);
    }
  }
}
