/** `extensions.schedules.settings`: retentionSweep turns on the daily sweep that deletes sessions idle past their agent's retention. */
export type SchedulesSettings = { retentionSweep?: boolean };

export const settingsProblems = (settings: unknown): string[] => {
  const s = (settings ?? {}) as Record<string, unknown>;
  const unknown = Object.keys(s).filter((k) => k !== 'retentionSweep').map((k) => `${k} is not a schedules setting`);
  return s.retentionSweep === undefined || typeof s.retentionSweep === 'boolean' ? unknown : [...unknown, 'retentionSweep must be true or false'];
};
