/** `extensions.github.settings`: implementerAgent is the agent a trusted `/fix` comment runs (absent, `/fix` does nothing); issueBase maps `owner/repo` to the branch `/implement` cuts from and opens its pull request against (a repository it does not list uses its default branch). */
export type GithubSettings = { implementerAgent?: string; issueBase?: Record<string, string> };

const named = (v: unknown) => v === undefined || (typeof v === 'string' && v.length > 0);
const REPO = /^[\w.-]+\/[\w.-]+$/;
const branches = (v: unknown) => v === undefined || (typeof v === 'object' && v !== null && !Array.isArray(v) && Object.entries(v).every(([repo, b]) => REPO.test(repo) && typeof b === 'string' && b.length > 0));

export const settingsProblems = (settings: unknown): string[] => {
  const s = (settings ?? {}) as Record<string, unknown>;
  const unknown = Object.keys(s).filter((k) => k !== 'implementerAgent' && k !== 'issueBase').map((k) => `${k} is not a github setting`);
  return [...unknown, ...(named(s.implementerAgent) ? [] : ['implementerAgent must be an agent name']), ...(branches(s.issueBase) ? [] : ['issueBase must map owner/repo to a branch name'])];
};
