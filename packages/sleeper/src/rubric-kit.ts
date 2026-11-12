import { registerSelector, registerSplitter, type CodeResult, type Question, type SelectorDecl } from '@quack/judge';

/** `n` of `of` units held; with no unit the check holds, and the detail is kept only for a shortfall. */
export const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });
export const parseCard = <C>(artifact: string) => JSON.parse(artifact) as C;

export const yes = (instructions: string): Question => ({ type: 'bool', instructions });
// A pitfall is asked as the defect, so "yes" flags it and the item holds on "no".
export const defect = (instructions: string): Question => ({ type: 'bool', instructions, pass: ['no'] });
export const SHARE = { name: 'share' } as const;

registerSelector('sleeper.closeCall', (_inputs, chunk, o) => JSON.stringify(chunk.meta?.[String((o as { field?: string }).field)] ?? null));
/** Context fields read off each item's meta, so a ported check's unit keeps its legacy fields at the top, item by item. */
export const metaFields = (fields: string[]): Record<string, SelectorDecl> => Object.fromEntries(fields.map((field) => [field, { name: 'sleeper.closeCall', field }]));

// A check that stays on the 27B reads its legacy unit: the record's `legacy[path]` as the piece, the rest of `legacy` around it.
registerSplitter('sleeper.legacy', (inputs, o) => (JSON.parse(inputs[String(o.from)]) as Array<{ id: string; legacy: Record<string, unknown> }>).map((r) => ({ id: r.id, text: JSON.stringify(r.legacy[String(o.path)]), meta: r.legacy })));
