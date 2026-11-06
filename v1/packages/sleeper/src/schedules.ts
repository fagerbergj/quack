/** `url` is a base URL that `fetch` resolves to the schedules app, whose routes are `/` and `/<id>`. */
export type ScheduleConfig = { url: string; fetch?: typeof fetch };

/** One job's registration: a single run at `at`, useful until `deadline`, or a cron in `tz`. */
export type ScheduleDef = { id: string; agent: string; sessionKey: string; input: object } & ({ at: number; deadline: number } | { cron: string; tz: string });

const iso = (ms: number) => new Date(ms).toISOString();

/** Registers a run with the schedules service; re-registering an id replaces it, so it is safe to repeat. Returns the id. */
export async function registerSchedule(config: ScheduleConfig, def: ScheduleDef): Promise<string> {
  const { id, agent, sessionKey, input } = def;
  const when = 'at' in def ? { times: [iso(def.at)], deadline: iso(def.deadline) } : { cron: def.cron, tz: def.tz };
  const res = await (config.fetch ?? fetch)(config.url, { method: 'POST', body: JSON.stringify({ id, agent, sessionKey, input: JSON.stringify(input), ...when }) });
  if (!res.ok) throw new Error(`schedules: registering ${id} returned ${res.status}`);
  return id;
}

/** Deletes a registration; 404 means it is already gone. */
export async function removeSchedule(config: ScheduleConfig, id: string): Promise<void> {
  const res = await (config.fetch ?? fetch)(`${config.url}/${encodeURIComponent(id)}`, { method: 'DELETE' });
  if (!res.ok && res.status !== 404) throw new Error(`schedules: removing ${id} returned ${res.status}`);
}
