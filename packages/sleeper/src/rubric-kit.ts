import { registerSelector, type CodeResult, type Evidence, type Question, type Rubric, type SelectorDecl } from '@quack/judge';

/** `n` of `of` units held; with no unit the check holds, and the detail is kept only for a shortfall. */
export const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });
export const parseCard = <C>(artifact: string) => JSON.parse(artifact) as C;
/** The job's facts, which every Sleeper evidence builder puts first. */
export const factsOf = <F>(ev: Evidence) => ev.facts[0].state as unknown as F;

export const yes = (instructions: string): Question => ({ type: 'bool', instructions });
// A pitfall is asked as the defect, so "yes" flags it and the item holds on "no".
export const defect = (instructions: string): Question => ({ type: 'bool', instructions, pass: ['no'] });
export const SHARE = { name: 'share' } as const;

registerSelector('sleeper.closeCall', (_inputs, chunk, o) => JSON.stringify(chunk.meta?.[String((o as { field?: string }).field)] ?? null));
/** Context fields read off each item's meta, so a ported check's unit keeps its legacy fields at the top, item by item. */
export const metaFields = (fields: string[]): Record<string, SelectorDecl> => Object.fromEntries(fields.map((field) => [field, { name: 'sleeper.closeCall', field }]));

/** A job's checks that read news, and what its verdict calls them when they go unchecked. */
export type NewsGate = { checks: string[]; what: string };

/** Checks that read news cannot be met without a news source, so they are scored only once one is wired; the notes say what was not checked. */
export function newsGated(rubric: Rubric, gate: NewsGate, hasNews: boolean): { rubric: Rubric; notes: string[] } {
  if (hasNews) return { rubric, notes: [] };
  return { rubric: { ...rubric, criteria: rubric.criteria.map((c) => ({ ...c, checks: c.checks.filter((k) => !gate.checks.includes(k.id)) })) }, notes: [`${gate.what} not checked: no news source`] };
}
