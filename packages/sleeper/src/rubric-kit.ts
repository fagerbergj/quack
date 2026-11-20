import type { CodeResult, Question } from '@quack/judge';

/** `n` of `of` units held; with no unit the check holds, and the detail is kept only for a shortfall. */
export const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });
export const parseCard = <C>(artifact: string) => JSON.parse(artifact) as C;

export const yes = (instructions: string): Question => ({ type: 'bool', instructions });
// A pitfall is asked as the defect, so "yes" flags it and the item holds on "no".
export const defect = (instructions: string): Question => ({ type: 'bool', instructions, pass: ['no'] });
export const SHARE = { name: 'share' } as const;
