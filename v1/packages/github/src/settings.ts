/** `extensions.github.settings`: implementerAgent is the agent a trusted `/fix` comment runs (absent, `/fix` does nothing); issueBase is the branch `/implement` cuts from and opens its pull request against (absent, the repository's default branch). */
export type GithubSettings = { implementerAgent?: string; issueBase?: string };

const named = (v: unknown) => v === undefined || (typeof v === 'string' && v.length > 0);

export const settingsProblems = (settings: unknown): string[] => {
  const s = (settings ?? {}) as Record<string, unknown>;
  const unknown = Object.keys(s).filter((k) => k !== 'implementerAgent' && k !== 'issueBase').map((k) => `${k} is not a github setting`);
  return [...unknown, ...(named(s.implementerAgent) ? [] : ['implementerAgent must be an agent name']), ...(named(s.issueBase) ? [] : ['issueBase must be a branch name'])];
};
