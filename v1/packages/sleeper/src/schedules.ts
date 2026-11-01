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
