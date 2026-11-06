import type { Decl } from './spec.ts';

/** Above this many boxes in view naming one type, it is a chip on each field instead of an arrow from each. */
export const HUB = 3;

/** One line of a box, `name type` (the name carries its `:`, `=` or `|`): the types it names drawn as arrows from this row (`edges`) or as clickable chips (`chips`). */
export type BoxRow = { name: string; type: string; edges: string[]; chips: string[] };
export type Box = { decl: Decl; focus: boolean; rows: BoxRow[] };
export type Focused = { boxes: Box[]; edges: Array<{ id: string; source: string; row: number; target: string }> };

const DRAWN = new Set<Decl['kind']>(['type', 'interface', 'enum', 'other']);
export const drawn = (decls: Decl[]) => decls.filter((d) => DRAWN.has(d.kind));

/** The declared names a type text mentions, once each, in order; never the declaration itself. */
export const namesIn = (type: string, names: Set<string>, self = '') => [...new Set(type.match(/[A-Za-z_$][\w$]*/g) ?? [])].filter((n) => n !== self && names.has(n));

// A box line is a label and a type: the top-level fields, a union's variants as `| …`, an enum's values, an interface's heritage, or the aliased type.
function lines(d: Decl): Array<[string, string]> {
  const top = d.rows.filter((r) => r.depth === 0).map((r): [string, string] => (r.where === 'one of' ? [`| ${r.name}`, r.type === 'object' ? '' : r.type] : d.kind === 'enum' ? [`${r.name} =`, r.type] : [`${r.name}${r.required ? '' : '?'}:`, r.type]));
  if (!top.length) return d.text ? [['=', d.text]] : [];
  return d.text ? [['extends', d.text], ...top] : top;
}

const usesOf = (d: Decl, names: Set<string>) => [...new Set(lines(d).flatMap(([, t]) => namesIn(t, names, d.name)))];

/** Where the diagram opens: the exported type that names the most others while nothing names it (the root), else the most-named exported type. */
export function defaultFocus(decls: Decl[]): string | undefined {
  const types = drawn(decls);
  const names = new Set(types.map((d) => d.name));
  const uses = new Map(types.map((d) => [d.name, usesOf(d, names)]));
  const named = (n: string) => types.filter((d) => uses.get(d.name)!.includes(n)).length;
  const pool = types.some((d) => d.exported) ? types.filter((d) => d.exported) : types;
  const best = (list: Decl[], score: (d: Decl) => number) => list.reduce<Decl | undefined>((b, d) => (!b || score(d) > score(b) ? d : b), undefined);
  const roots = pool.filter((d) => named(d.name) === 0 && uses.get(d.name)!.length > 0);
  return (best(roots, (d) => uses.get(d.name)!.length) ?? best(pool, (d) => named(d.name)))?.name;
}

/** One hop around `focus`: it, the types it names and the types that name it. A field's reference to a type in view is an arrow from that
 * field, unless more than HUB boxes around name that type (not the focus); such a hub, and any type out of view, is a chip and has no box. */
export function focusView(decls: Decl[], focus: string, hub = HUB): Focused {
  const types = drawn(decls);
  const names = new Set(types.map((d) => d.name));
  const centre = types.find((d) => d.name === focus);
  if (!centre) return { boxes: [], edges: [] };
  const around = new Set([focus, ...usesOf(centre, names), ...types.filter((d) => usesOf(d, names).includes(focus)).map((d) => d.name)]);
  const near = types.filter((d) => around.has(d.name));
  const namedBy = (n: string) => near.filter((d) => usesOf(d, names).includes(n)).length;
  const arrow = (n: string) => around.has(n) && (n === focus || namedBy(n) <= hub);
  // A hub is only ever a chip, so its own box would float unconnected.
  const inView = near.filter((d) => arrow(d.name));
  const edges: Focused['edges'] = [];
  const boxes = inView.map((decl) => ({
    decl,
    focus: decl.name === focus,
    rows: lines(decl).map(([name, type], row) => {
      const refs = namesIn(type, names, decl.name);
      for (const target of refs.filter(arrow)) edges.push({ id: `${decl.name}:${row}>${target}`, source: decl.name, row, target });
      return { name, type, edges: refs.filter(arrow), chips: refs.filter((n) => !arrow(n)) };
    }),
  }));
  return { boxes, edges };
}
