import type { Break } from './oasdiff.ts';
import type { Breaking } from './spec.ts';

/** A breaking entry as stored: specs sealed before contract.spec v2 may say `change` for what and name their callers in one string. */
export type Listed = Partial<Omit<Breaking, 'callers'>> & { change?: string; callers?: string[] | string };
type Found = Pick<Break, 'text'> & Partial<Break>;

const esc = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const says = (text: string, name: string) => new RegExp(`(^|[^\\w$])${esc(name)}($|[^\\w$])`).test(text);
const lastIdent = (p: string) => [p.split('/').filter(Boolean).at(-1) ?? ''].filter((s) => /^[A-Za-z_$][\w$]*$/.test(s));

/** How a person knows a break's place: a route by its method and path, an export by its name, a schema place by its pointer. */
export const where = (b: Found) => (b.operation ? `${b.operation} ${b.path ?? ''}` : (b.path ?? '').replace(/^\/(?=[A-Za-z_$][\w$]*$)/, '')).trim() || b.text;

// The names prose would use for a break: its path, the path's last identifier (an export, a property), and the last identifier of
// each name the checker's text quotes (a REST property or enum value, a JSON Schema property), since a route's break is named by its field.
function names(b: Found): string[] {
  const path = b.path ?? '';
  const quoted = [...b.text.matchAll(/`([^`\s]+)`|"([^"\s]+)"/g)].flatMap((m) => lastIdent(m[1] ?? m[2]));
  return [...new Set([...(path.length > 1 ? [path] : []), ...lastIdent(path), ...quoted])];
}

// A schema's root has no name prose could use, so any listed entry accounts for a nameless break there.
const namedIn = (text: string, b: Found) => { const ns = names(b); return ns.length ? ns.some((n) => says(text, n)) : b.path === '/' && text.trim() !== ''; };

/** The places the checker found a break that no listed entry names as a whole word in its what, callers or migration: one entry may
 * name several. A text match, not a proof; the web page's Approval.tsx mirrors it, both held to test-fixtures/break-names.json. */
export function unnamedBreaks(listed: Listed[], found: Found[]): string[] {
  const text = listed.map((b) => [b.what ?? b.change ?? '', ...[b.callers ?? []].flat(), b.migration ?? ''].join('\n')).join('\n');
  return [...new Set(found.filter((b) => !namedIn(text, b)).map(where))];
}

// A safe note names a place by its path or an export's or schema property's name; a route's last segment is too common a word.
const placeNames = (b: Found) => [...((b.path ?? '').length > 1 ? [b.path!] : []), ...(b.operation ? [] : lastIdent(b.path ?? ''))];

/** Each safe note that names an unnamed place: the spec's only word on a break the checker found is that it is safe. */
export function safeClaims(safe: string[], found: Found[], unnamed: string[]): Array<{ note: number; places: string[] }> {
  const open = found.filter((b) => unnamed.includes(where(b)));
  return safe.map((s, note) => ({ note, places: [...new Set(open.filter((b) => placeNames(b).some((n) => says(s, n))).map(where))] })).filter((c) => c.places.length);
}
