/** `services.github.settings`: implementerAgent is the agent a trusted `/fix` comment runs; absent, `/fix` does nothing. */
export type GithubSettings = { implementerAgent?: string };

export const settingsProblems = (settings: unknown): string[] => {
  const s = (settings ?? {}) as Record<string, unknown>;
  const unknown = Object.keys(s).filter((k) => k !== 'implementerAgent').map((k) => `${k} is not a github setting`);
  return s.implementerAgent === undefined || (typeof s.implementerAgent === 'string' && s.implementerAgent) ? unknown : [...unknown, 'implementerAgent must be an agent name'];
};
