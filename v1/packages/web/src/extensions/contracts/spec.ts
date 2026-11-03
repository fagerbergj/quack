// Local copy of the ContractSpec type from the contracts package (#2065 backend lane); switch to its import once that package merges.
export type Source = { source: 'answer' | 'guide' | 'code'; text: string; ref: string };
export type Choice = { id: string; pointer: string; decision: string; why: Source[] };
export type ContractSpec = {
  kind: 'rest';
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

export function parseSpec(content: string): ContractSpec | undefined {
  try {
    const s = JSON.parse(content) as Partial<ContractSpec>;
    return s && s.kind === 'rest' && typeof s.document === 'object' && Array.isArray(s.choices) && s.intent ? (s as ContractSpec) : undefined;
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

// Follows a local $ref; the pointer then names the referenced schema, where its choices live.
export function resolve(doc: Json, schema: Json | undefined, at: string): { schema: Json; ptr: string } | undefined {
  if (!schema) return undefined;
  const ref = schema.$ref;
  if (typeof ref !== 'string' || !ref.startsWith('#/')) return { schema, ptr: at };
  const target = ref.slice(2).split('/').map((s) => s.replace(/~1/g, '/').replace(/~0/g, '~')).reduce<Json | undefined>((n, k) => n?.[k], doc);
  return resolve(doc, target, ref.slice(1));
}

export type Field = { name: string; type: string; required: boolean; desc?: string; depth: number; ptr: string; where?: string; facts: Array<[string, string]> };

const typeOf = (s: Json) => (s.format as string | undefined) ?? (Array.isArray(s.type) ? s.type.join(' | ') : (s.type as string | undefined) ?? 'any');
const show = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v));

function facts(s: Json): Field['facts'] {
  return [['default', s.default], ['example', s.example], ['one of', s.enum]]
    .filter(([, v]) => v !== undefined)
    .map(([k, v]) => [k as string, Array.isArray(v) ? v.map(show).join(' · ') : show(v)]);
}

// Flattens a schema into the rows its tree shows; an array of objects continues into the item's properties.
export function fields(doc: Json, schema: Json | undefined, at: string, depth = 0): Field[] {
  const r = resolve(doc, schema, at);
  if (!r) return [];
  const base = resolve(doc, r.schema.type === 'array' ? r.schema.items : undefined, `${r.ptr}/items`) ?? r;
  const required = new Set<string>(base.schema.required ?? []);
  return Object.entries((base.schema.properties ?? {}) as Json).flatMap(([name, child]) => {
    const own = `${base.ptr}/properties/${esc(name)}`;
    const c = resolve(doc, child, own)!;
    const f: Field = { name, type: typeOf(c.schema), required: required.has(name), desc: c.schema.description, depth, ptr: own, facts: facts(c.schema) };
    return [f, ...fields(doc, c.schema, own, depth + 1)];
  });
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
