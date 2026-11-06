import { createContext, useContext } from 'react';
import { fields, operations, paramFields, resolve, type ContractSpec, type Field, type Json, type Model, type Operation } from './spec.ts';

/** How an element of the shown version differs from the version it is compared with. */
export type Mark = 'added' | 'changed';
/** Each element the page draws, by the JSON pointer it is drawn at: a label for a removal and a fingerprint of what it shows. */
export type Units = Map<string, { label: string; fp: string }>;
export type Diff = { marks: Map<string, Mark>; removed: string[] };

const fp = (v: unknown) => JSON.stringify(v ?? null);
const fieldFp = (f: Field) => fp([f.type, f.required, f.desc, f.where, f.facts]);
const addRows = (out: Units, rows: Field[], owner: string) => rows.forEach((f) => out.set(f.ptr, { label: `${owner} › ${f.name}`, fp: fieldFp(f) }));

const JSON_BODY = 'content/application~1json/schema';
const bodyOf = (part: Json | undefined) => (part?.content?.['application/json'] as Json | undefined)?.schema;

/** The pointers an operation draws: itself, its auth, parameters, request and response fields, and each response. */
export function opUnits(doc: Json, o: Operation): Units {
  const label = `${o.method} ${o.path}`;
  const out: Units = new Map([[o.ptr, { label, fp: fp([o.op.summary, o.op.description, o.op.operationId, o.op.tags, o.op.deprecated]) }]]);
  if (o.op.security) out.set(`${o.ptr}/security`, { label: `${label} › auth`, fp: fp(o.op.security) });
  addRows(out, paramFields(doc, o), label);
  const schema = (raw: Json | undefined, at: string) => {
    const r = resolve(doc, raw, at);
    if (r) addRows(out, fields(doc, r.schema, r.ptr), label);
  };
  schema(bodyOf(o.op.requestBody), `${o.ptr}/requestBody/${JSON_BODY}`);
  for (const [code, res] of Object.entries((o.op.responses ?? {}) as Json)) {
    out.set(`${o.ptr}/responses/${code}`, { label: `${label} › ${code}`, fp: fp([res.description, Object.keys(res.content ?? {})]) });
    schema(bodyOf(res), `${o.ptr}/responses/${code}/${JSON_BODY}`);
  }
  return out;
}

function documentUnits(spec: ContractSpec, model?: Model): Units {
  const doc = spec.document as Json;
  if (spec.kind === 'rest') return new Map(operations(doc).flatMap((o) => [...opUnits(doc, o)]));
  const out: Units = new Map();
  if (spec.kind === 'json-schema') addRows(out, fields(doc, doc, ''), 'field');
  for (const d of model?.decls ?? []) {
    out.set(`/${d.name}`, { label: d.name, fp: fp([d.kind, d.exported, d.generics, d.doc, d.text]) });
    addRows(out, d.rows, d.name);
  }
  return out;
}

/** What a version draws, by pointer: the title and purpose, then the kind's elements. A TypeScript spec needs its parsed model. */
export function units(spec: ContractSpec, model?: Model): Units {
  return new Map([['#title', { label: 'Title', fp: fp(spec.title) }], ['#purpose', { label: 'What it does', fp: fp(spec.intent) }], ...documentUnits(spec, model)]);
}

/** What changed from `was` to `now`: each element of `now` that is new or draws differently, and the labels of those `now` lost. */
export function diffUnits(was: Units, now: Units): Diff {
  const marks = new Map<string, Mark>();
  for (const [ptr, u] of now) {
    const before = was.get(ptr);
    if (!before) marks.set(ptr, 'added');
    else if (before.fp !== u.fp) marks.set(ptr, 'changed');
  }
  return { marks, removed: [...was].filter(([ptr]) => !now.has(ptr)).map(([, u]) => u.label) };
}

/** The marks of the comparison on screen; outside one nothing is marked. */
export const DiffContext = createContext<Map<string, Mark> | undefined>(undefined);
export const useMark = (ptr: string) => useContext(DiffContext)?.get(ptr);
/** Whether any of the pointers is marked, for an element that stands for several (a route in the list). */
export const useAnyMark = (ptrs: Iterable<string>) => {
  const marks = useContext(DiffContext);
  return !!marks && [...ptrs].some((p) => marks.has(p));
};
