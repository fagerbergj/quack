/** `services.sleeper.settings`: the league the service acts on, and the owner whose lineup, waiver and trade jobs it schedules (absent: no jobs are registered). */
export type SleeperSettings = { league: string; owner?: string };

export const settingsProblems = (settings: unknown): string[] => {
  const s = (settings ?? {}) as Record<string, unknown>;
  const unknown = Object.keys(s).filter((k) => k !== 'league' && k !== 'owner').map((k) => `${k} is not a sleeper setting`);
  const text = (k: string) => (typeof s[k] === 'string' && s[k] ? [] : [`${k} must be a non-empty string`]);
  return [...unknown, ...text('league'), ...(s.owner === undefined ? [] : text('owner'))];
};
