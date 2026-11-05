// Local copy of the ContractSpec type from the contracts package (#2065 backend lane); switch to its import once that package merges.
export type Source = { source: 'answer' | 'guide' | 'code'; text: string; ref: string };
export type Choice = { id: string; pointer: string; decision: string; why: Source[] };
export type ContractSpec = {
  kind: 'rest' | 'json-schema' | 'typescript';
  title: string;
  version: number;
  intent: { purpose: string; nonGoals: string[]; audience: string; consumers: string[]; guarantees: string[] };
  document: unknown;
  choices: Choice[];
  changes?: Array<{ choice: string; was: string; now: string; why: string }>;
  compatibility?: { breaking: Array<{ what: string; callers: string[] }>; safe: string[] };
};

// An OpenAPI document is walked loosely; the backend lane's checks own its validity.
export type Json = { [k: string]: any };

const isObject = (v: unknown): v is Json => typeof v === 'object' && v !== null;

// A spec whose document has no paths object is not drawable; the card then falls back to plain text.
export function parseSpec(content: string): ContractSpec | undefined {
  try {
    const s = JSON.parse(content) as Partial<ContractSpec>;
    const drawable = s.kind === 'typescript' ? typeof s.document === 'string' : s.kind === 'json-schema' ? isObject(s.document) : s.kind === 'rest' && isObject(s.document) && isObject((s.document as Json).paths);
    return isObject(s) && drawable && Array.isArray(s.choices) && isObject(s.intent) ? (s as ContractSpec) : undefined;
  } catch {
    return undefined;
  }
}

const esc = (s: string) => s.replace(/~/g, '~0').replace(/\//g, '~1');
export const ptr = (...segs: string[]) => segs.map((s) => `/${esc(s)}`).join('');

export type Operation = { method: string; path: string; ptr: string; op: Json };
const METHODS = ['get', 'put', 'post', 'delete', 'patch'];

export function operations(doc: Json): Operation[] {
  return Object.entries((doc.paths ?? {}) as Json).flatMap(([path, item]) =>
    METHODS.filter((m) => item[m]).map((m) => ({ method: m.toUpperCase(), path, ptr: ptr('paths', path, m), op: item[m] as Json })),
  );
}

// Follows a local $ref; the pointer then names the referenced schema, where its choices live. A chain of refs that loops resolves to nothing.
export function resolve(doc: Json, schema: Json | undefined, at: string, seen: string[] = []): { schema: Json; ptr: string } | undefined {
  if (!schema) return undefined;
  const ref = schema.$ref;
  if (typeof ref !== 'string' || !ref.startsWith('#/')) return { schema, ptr: at };
  if (seen.includes(ref)) return undefined;
  const target = ref.slice(2).split('/').map((s) => s.replace(/~1/g, '/').replace(/~0/g, '~')).reduce<Json | undefined>((n, k) => n?.[k], doc);
  return resolve(doc, target, ref.slice(1), [...seen, ref]);
}

export type Field = { name: string; type: string; required: boolean; desc?: string; depth: number; ptr: string; where?: string; facts: Array<[string, string]> };

/** One declaration of a code contract, whatever language it was parsed from, as its box and card show it: fields, parameters or variants as rows, else its type as text. */
export type Decl = {
  name: string;
  kind: 'type' | 'interface' | 'function' | 'const' | 'other';
  exported: boolean;
  generics?: string;
  doc?: string;
  rows: Field[];
  // The type when there are no rows to list, or what an intersection or interface adds beside its own fields.
  text?: string;
  uses: string[];
};
export type Model = { imports: Array<{ from: string; names: string[] }>; decls: Decl[] };

const enumType = (s: Json) => (Array.isArray(s.enum) ? [...new Set(s.enum.map((v: unknown) => (v === null ? 'null' : typeof v)))].join(' | ') : undefined);
const typeOf = (s: Json) => (s.format as string | undefined) ?? (Array.isArray(s.type) ? s.type.join(' | ') : (s.type as string | undefined) ?? enumType(s) ?? 'any');
const show = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v));

function facts(s: Json): Field['facts'] {
  return [['default', s.default], ['example', s.example ?? (Array.isArray(s.examples) ? s.examples[0] : undefined)], ['one of', s.enum]]
    .filter(([, v]) => v !== undefined)
    .map(([k, v]) => [k as string, Array.isArray(v) ? v.map(show).join(' · ') : show(v)]);
}

type Step = { r: { schema: Json; ptr: string }; seen: string[] } | { cycle: string } | undefined;

// One hop into a schema; a $ref already on the way down is a cycle, named by its last segment.
function step(doc: Json, raw: Json | undefined, at: string, seen: string[]): Step {
  const ref = typeof raw?.$ref === 'string' ? raw.$ref : undefined;
  if (ref && seen.includes(ref)) return { cycle: ref.split('/').pop()! };
  const r = resolve(doc, raw, at);
  return r && { r, seen: ref ? [...seen, ref] : seen };
}

const cycleRow = (name: string, type: string, depth: number, ptr: string): Field => ({ name, type, required: false, desc: `${type}, see above`, depth, ptr, facts: [] });

// Flattens a schema into the rows its tree shows; an array of objects continues into the item's properties, and a schema met again further down is a named reference.
export function fields(doc: Json, schema: Json | undefined, at: string, depth = 0, seen: string[] = []): Field[] {
  const top = step(doc, schema, at, seen);
  if (!top || 'cycle' in top) return [];
  const items = top.r.schema.type === 'array' ? step(doc, top.r.schema.items, `${top.r.ptr}/items`, top.seen) : undefined;
  if (items && 'cycle' in items) return [cycleRow('each item', items.cycle, depth, `${top.r.ptr}/items`)];
  const base = items ?? top;
  const required = new Set<string>(base.r.schema.required ?? []);
  return Object.entries((base.r.schema.properties ?? {}) as Json).flatMap(([name, child]) => {
    const own = `${base.r.ptr}/properties/${esc(name)}`;
    const c = step(doc, child, own, base.seen);
    if (!c) return [];
    if ('cycle' in c) return [{ ...cycleRow(name, c.cycle, depth, own), required: required.has(name) }];
    const f: Field = { name, type: typeOf(c.r.schema), required: required.has(name), desc: c.r.schema.description, depth, ptr: own, facts: facts(c.r.schema) };
    return [f, ...fields(doc, c.r.schema, own, depth + 1, c.seen)];
  });
}

const JSON_SCHEMA = 'content/application~1json/schema';

/** Every pointer the REST page draws an element for (operation, auth, parameter, response, schema field), so a decision anywhere else goes to the title. */
export function restPointers(doc: Json, ops: Operation[]): Set<string> {
  const out = new Set<string>();
  const schema = (raw: Json | undefined, at: string) => {
    const r = resolve(doc, raw, at);
    if (r) for (const f of fields(doc, r.schema, r.ptr)) out.add(f.ptr);
  };
  for (const o of ops) {
    out.add(o.ptr);
    if (Object.keys((o.op.security?.[0] ?? {}) as Json).length) out.add(`${o.ptr}/security`);
    ((o.op.parameters ?? []) as Json[]).forEach((_, i) => out.add(`${o.ptr}/parameters/${i}`));
    schema((o.op.requestBody?.content?.['application/json'] as Json | undefined)?.schema, `${o.ptr}/requestBody/${JSON_SCHEMA}`);
    for (const [code, res] of Object.entries((o.op.responses ?? {}) as Json)) {
      out.add(`${o.ptr}/responses/${code}`);
      schema((res.content?.['application/json'] as Json | undefined)?.schema, `${o.ptr}/responses/${code}/${JSON_SCHEMA}`);
    }
  }
  return out;
}

// Plain YAML for the downloadable standard document; strings and odd keys are JSON-quoted, which YAML accepts.
export function toYaml(v: unknown, pad = ''): string {
  const leaf = (x: unknown) => x === null || typeof x !== 'object' || Object.keys(x).length === 0;
  const lit = (x: unknown) => (Array.isArray(x) ? '[]' : x !== null && typeof x === 'object' ? '{}' : typeof x === 'string' ? JSON.stringify(x) : String(x));
  if (Array.isArray(v)) return v.map((x) => (leaf(x) ? `${pad}- ${lit(x)}` : `${pad}-\n${toYaml(x, `${pad}  `)}`)).join('\n');
  if (leaf(v)) return `${pad}${lit(v)}`;
  return Object.entries(v as object).map(([k, x]) => {
    const key = /^[A-Za-z_][\w-]*$/.test(k) ? k : JSON.stringify(k);
    return leaf(x) ? `${pad}${key}: ${lit(x)}` : `${pad}${key}:\n${toYaml(x, `${pad}  `)}`;
  }).join('\n');
}
