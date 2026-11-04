import type { Break } from './oasdiff.ts';
import { esc, subschemas } from './json-schema-rules.ts';

type Schema = Record<string, unknown>;
const isObj = (v: unknown): v is Schema => typeof v === 'object' && v !== null && !Array.isArray(v);
const brk = (id: string, text: string, path: string): Break => ({ id, text, operation: '', path: path || '/' });
const list = (v: unknown): unknown[] => (Array.isArray(v) ? v : v === undefined ? [] : [v]);
const props = (s: Schema) => (isObj(s.properties) ? s.properties : {});
const allowsExtra = (s: Schema) => s.additionalProperties !== false && s.unevaluatedProperties !== false;

// integer is a subset of number, so a number schema accepts everything an integer one did.
const accepts = (types: unknown[], t: unknown) => types.includes(t) || (t === 'integer' && types.includes('number'));

function typeBreaks(a: Schema, b: Schema, at: string): Break[] {
  const out: Break[] = [];
  if (a.type !== undefined) {
    const lost = list(a.type).filter((t) => !accepts(list(b.type), t));
    if (b.type !== undefined && lost.length) out.push(brk('type-narrowed', `type no longer accepts ${lost.join(', ')}, so documents that used it are now invalid`, at));
  }
  const was = list(a.enum), now = list(b.enum);
  const gone = was.filter((v) => !now.some((n) => JSON.stringify(n) === JSON.stringify(v)));
  if (b.enum !== undefined && (a.enum === undefined || gone.length)) out.push(brk('enum-narrowed', a.enum === undefined ? 'a value list now restricts a previously free field' : `enum no longer allows ${gone.map((v) => JSON.stringify(v)).join(', ')}`, at));
  return out;
}

function objectBreaks(a: Schema, b: Schema, at: string): Break[] {
  const out: Break[] = [];
  for (const n of Object.keys(props(a))) if (!Object.hasOwn(props(b), n)) out.push(brk('property-removed', `property "${n}" is removed, so readers that use it lose it (writers that send it are rejected if unknown properties are closed)`, `${at}/properties/${esc(n)}`));
  const had = new Set(list(a.required));
  for (const n of list(b.required)) if (!had.has(n)) out.push(brk('property-required', `"${String(n)}" is now required, so documents without it are invalid`, `${at}/required`));
  if (allowsExtra(a) && !allowsExtra(b)) out.push(brk('additional-properties-closed', 'unknown properties are no longer allowed, so documents that carry extra fields are now invalid', at));
  return out;
}

/** Breaking changes between two JSON Schema versions, compared at the same pointer. */
export async function schemaBreaks(prev: unknown, next: unknown): Promise<Break[]> {
  const after = new Map(subschemas(next).map(([s, at]) => [at, s]));
  return [...subschemas(prev)].flatMap(([a, at]) => {
    const b = after.get(at);
    return b ? [...typeBreaks(a, b, at), ...objectBreaks(a, b, at)] : [];
  });
}
