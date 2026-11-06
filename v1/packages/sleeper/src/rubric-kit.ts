import type { CodeResult, Evidence, Rubric } from '@quack/judge';

/** `n` of `of` units held; with no unit the check holds, and the detail is kept only for a shortfall. */
export const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });
export const parseCard = <C>(artifact: string) => JSON.parse(artifact) as C;
/** The job's facts, which every Sleeper evidence builder puts first. */
export const factsOf = <F>(ev: Evidence) => ev.facts[0].state as unknown as F;

/** Checks that read news cannot be met without a news source, so they are scored only once one is wired; the notes say what was not checked. */
export function newsGated(rubric: Rubric, newsChecks: string[], hasNews: boolean, what: string): { rubric: Rubric; notes: string[] } {
  if (hasNews) return { rubric, notes: [] };
  return { rubric: { ...rubric, criteria: rubric.criteria.map((c) => ({ ...c, checks: c.checks.filter((k) => !newsChecks.includes(k.id)) })) }, notes: [`${what} not checked: no news source`] };
}
