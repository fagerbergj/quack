import type { AggregateDecl } from './declaration.ts';

/** An item's answer: true or false once a resolver is confident, undefined while it is not. */
export type Tri = boolean | undefined;

const bounds = (lo: number, hi: number, need: number): Tri => (lo >= need ? true : hi < need ? false : undefined);

/** The three-valued fan-in: decided once no assignment of the undecided items can change the result, undefined while one can. */
export function fold(ag: AggregateDecl, items: Tri[]): Tri {
  const n = items.length;
  const t = items.filter((v) => v === true).length;
  const u = items.filter((v) => v === undefined).length;
  if (ag.name === 'all') return bounds(t, t + u, n);
  if (ag.name === 'any') return bounds(t, t + u, 1);
  if (ag.name === 'atLeast') return bounds(t, t + u, ag.k ?? 1);
  return n ? bounds(t / n, (t + u) / n, ag.min ?? 1) : true;
}

/** Whether the fold reports a share of items (of groups, for a grouped fold) rather than a decided boolean. */
export const fractional = (ag: AggregateDecl) => ag.name === 'share' || ag.name === 'atLeast' || (ag.name === 'grouped' && (ag.outer === 'share' || ag.outer === 'atLeast'));

/** A grouped fold's outer rule, as the flat fold it runs over the groups' results. */
export const outerOf = (ag: AggregateDecl): AggregateDecl => ({ name: ag.outer!, k: ag.k, min: ag.min });

/** Each group's inner fold over its items' answers, by group key in the order the groups first appear. */
export function groupResults(ag: AggregateDecl, keys: string[], items: Tri[]): Map<string, Tri> {
  const members = new Map<string, Tri[]>();
  keys.forEach((k, i) => members.set(k, [...(members.get(k) ?? []), items[i]]));
  return new Map([...members].map(([k, tris]) => [k, fold({ name: ag.inner! }, tris)]));
}

/** The check's met for a decided fold: a positive check holds when the fold is true, a pitfall is present when it is false. */
export function metOf(ag: AggregateDecl, value: boolean, items: Tri[], pitfall: boolean): boolean | number {
  if (!fractional(ag)) return pitfall ? !value : value;
  // A pitfall's share is of the items where the claim failed, so it subtracts where present.
  return items.filter((v) => v === !pitfall).length / items.length;
}
