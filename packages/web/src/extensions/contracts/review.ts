import type { Change, Diff, Mark } from './diff.ts';
import { listedBreaks, type ContractSpec } from './spec.ts';

const LONG = 140;
const flat = (t: string) => t.replace(/\s+/g, ' ').trim();
const closed = (s: string) => (s.match(/`/g)?.length ?? 0) % 2 === 0;
const sentences = (t: string) => flat(t).split(/(?<=[.!?])\s+/).filter(Boolean);

/** A text's first sentence, cut at a colon, dash or semicolon when it runs long; '' when no clean cut exists, and the caller offers the text on demand. */
export function lede(text = ''): string {
  const first = sentences(text)[0] ?? '';
  if (first.length <= LONG && closed(first)) return first;
  const cut = first.split(/:\s| - | — |; /)[0]!;
  return cut.length >= 18 && cut.length <= LONG && closed(cut) ? `${cut.replace(/[.,]$/, '')}.` : '';
}

/** Whether the whole text says more than the part shown. */
export const saysMore = (text: string | undefined, shown: string) => flat(text ?? '').length > shown.length + 1;

/** A text change in whole sentences: the first sentence only the old text has, the first only the new one has, and whether more changed. */
export function shortChange(was: string, now: string) {
  const a = sentences(was), b = sentences(now);
  const gone = a.filter((s) => !b.includes(s)), added = b.filter((s) => !a.includes(s));
  return { was: gone[0] ?? '', now: added[0] ?? '', more: gone.length > 1 || added.length > 1 };
}

/** One change against the compared version, as the list, the marks and Next change use it: where it is drawn (`target`) and one line saying what changed. */
export type Item = { ptr: string; target: string; mark: Mark | 'removed'; name: string; line: string; rows: Change['rows']; breaking: boolean; callers: string[] };

// Intent parts each have their own place on the page, so each is a change of its own.
const INTENT: Record<string, [string, string]> = {
  purpose: ['Purpose', '#purpose'], 'non-goals': ['Out of scope', '#non-goals'], audience: ['Audience', '#audience'], consumers: ['Used by', '#audience'], guarantees: ['Guarantees', '#guarantees'],
};
const PART: Record<string, string> = { doc: 'description', exported: 'export', in: 'place' };
const keyOf = (c: Change) => (c.ptr === '#purpose' ? `#purpose/${c.part}` : c.ptr);
// An element new or removed with its parent says nothing its parent's line does not.
const implied = (c: Change, all: Change[]) => c.mark !== 'changed' && all.some((o) => o.mark === c.mark && c.ptr.startsWith(`${o.ptr}/`));

function nameOf(kind: ContractSpec['kind'], c: Change) {
  if (c.ptr === '#purpose') return INTENT[c.part!]?.[0] ?? c.part!;
  return kind === 'rest' ? c.label : c.label.replaceAll(' › ', '.');
}

type Lookup = Diff['unit'];

// A declaration names its own kind; one of its rows is a parameter of a function and a field of anything else.
function nounOf(kind: ContractSpec['kind'], c: Change, unit: Lookup) {
  const own = c.rows.find((r) => r.name === 'kind')?.[c.mark === 'removed' ? 'was' : 'now'];
  if (own) return own === 'other' ? '' : `${own} `;
  if (kind === 'rest' || c.ptr.startsWith('#')) return '';
  const segs = c.ptr.split('/');
  return segs.length === 3 && unit(`/${segs[1]}`)?.parts.kind === 'function' ? 'parameter ' : 'field ';
}

function lineOf(kind: ContractSpec['kind'], name: string, cs: Change[], unit: Lookup): string {
  const c = cs[0]!;
  if (c.mark !== 'changed') return `${c.mark === 'added' ? 'New' : 'Removed'} ${nounOf(kind, c, unit)}${name}`;
  if (c.ptr === '#purpose' || c.ptr === '#title') return `${name} changed`;
  if (cs.length === 1 && c.part === 'required') return `${name} is now ${c.rows[0]!.now}`;
  return `${name}: ${[...new Set(cs.map((x) => PART[x.part!] ?? x.part))].join(' and ')} changed`;
}

// A removed element is shown where its nearest surviving parent is drawn; an intent part emptied has no place left, so the list stands in.
function targetOf(c: Change, unit: Lookup): string {
  if (c.ptr === '#purpose') return c.rows[0]?.now ? (INTENT[c.part!]?.[1] ?? '#purpose') : '#changes';
  if (c.mark !== 'removed') return c.ptr;
  for (let p = parent(c.ptr); p; p = parent(p)) if (unit(p)) return p;
  return '#changes';
}
const parent = (ptr: string) => ptr.slice(0, Math.max(0, ptr.lastIndexOf('/')));

const esc = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
// A whole name, not the prefix of a longer dotted one: `CheckResult` is not named by `CheckResult.resolvers`.
const names = (text: string, name: string) => new RegExp(`(^|[^\\w$.])${esc(name)}(?![\\w$]|\\.[A-Za-z_$])`).test(text);
const word = (text: string, w: string) => new RegExp(`(^|[^\\w$])${esc(w)}(?![\\w$])`).test(text);
const GENERIC = new Set(['id', 'at', 'name', 'type', 'key', 'value', 'data', 'items', 'body']);

// A REST or JSON Schema breadcrumb never appears whole in prose, so its element's own name counts; a short or generic one only beside its parent's.
function named(kind: ContractSpec['kind'], text: string, name: string) {
  if (kind === 'typescript') return names(text, name);
  const segs = name.split(kind === 'rest' ? ' › ' : '.');
  const own = segs.at(-1)!, up = segs.at(-2);
  const loose = own.length < 4 || GENERIC.has(own.toLowerCase());
  return word(text, own) && (!loose || (!!up && word(text, up)));
}

/** One change per changed element or intent part: the document's in drawing order, its removals, then the intent's. A change is breaking
 * when a listed break names it or the checker found one at its pointer: a text match, not a proof. */
export function changeItems(diff: Diff, spec: ContractSpec, found: Array<{ path?: string }> = []): Item[] {
  const listed = listedBreaks(spec);
  const groups = Map.groupBy(diff.changes.filter((c) => !implied(c, diff.changes)), keyOf);
  return [...groups.values()].sort((a, b) => Number(a[0]!.ptr.startsWith('#')) - Number(b[0]!.ptr.startsWith('#'))).map((cs) => {
    const c = cs[0]!, name = nameOf(spec.kind, c);
    const hit = c.ptr.startsWith('#') ? [] : listed.filter((b) => named(spec.kind, b.what, name));
    const rows = c.mark === 'changed' ? cs.map((x) => ({ name: PART[x.part!] ?? x.part, was: x.rows[0]!.was, now: x.rows[0]!.now })) : c.rows;
    // A new element breaks no reader, whatever prose mentions it.
    const breaking = c.mark !== 'added' && (hit.length > 0 || found.some((f) => f.path === c.ptr));
    return { ptr: c.ptr, target: targetOf(c, diff.unit), mark: c.mark, name, line: lineOf(spec.kind, name, cs, diff.unit), rows, breaking, callers: breaking ? hit.flatMap((b) => b.callers) : [] };
  });
}

/** The diff's marks with each breaking change marked as such. */
export function markBreaks(marks: Map<string, Mark>, items: Item[]): Map<string, Mark> {
  const out = new Map(marks);
  for (const i of items) if (i.breaking && i.mark !== 'removed') out.set(i.ptr, 'breaking');
  return out;
}

/** The changes at or under a pointer, for an element that stands for several (a declaration, a box). */
export const itemsUnder = (items: Item[], ptr: string) => items.filter((i) => i.ptr === ptr || i.ptr.startsWith(`${ptr}/`));

/** The strongest mark of a set of changes: a break outranks any other change. */
export const worst = (items: Item[]): Mark | undefined => (items.some((i) => i.breaking) ? 'breaking' : items.length ? 'changed' : undefined);
