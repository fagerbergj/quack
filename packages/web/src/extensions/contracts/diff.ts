import { createContext, useContext } from 'react';
import { fields, operations, paramFields, resolve, type ContractSpec, type Decl, type Field, type Json, type Model, type Operation } from './spec.ts';

/** How an element of the shown version differs from the version it is compared with. */
export type Mark = 'added' | 'changed';
/** Each element the page draws, by the JSON pointer it is drawn at: where it is, as a breadcrumb, and the text of each of its parts. */
export type Unit = { label: string; parts: Record<string, string> };
export type Units = Map<string, Unit>;
/** One change as the list shows it: a changed part of an element, or a whole element new or removed, each part's text before and after. */
export type Change = { ptr: string; mark: Mark | 'removed'; crumb: string; rows: Array<{ name?: string; was: string; now: string }> };
export type Diff = { marks: Map<string, Mark>; changes: Change[] };

const text = (v: unknown): string => (v == null || v === false ? '' : typeof v === 'string' ? v : Array.isArray(v) ? v.map(text).join(', ') : v === true ? 'yes' : JSON.stringify(v));
// Parts with no text are dropped, so a part that appears or goes reads as a change from or to nothing.
const parts = (o: Record<string, unknown>) => Object.fromEntries(Object.entries(o).map(([k, v]) => [k, text(v)]).filter(([, v]) => v));
const fieldParts = (f: Field) => parts({ type: f.type, required: f.required ? 'required' : 'optional', in: f.where, description: f.desc, ...Object.fromEntries(f.facts) });

// A row's breadcrumb names its parents too: rows come depth-first, so the path so far is the parents.
function addRows(out: Units, rows: Field[], owner: string[]) {
  const path: string[] = [];
  for (const f of rows) {
    path.length = f.depth;
    path.push(f.name);
    out.set(f.ptr, { label: [...owner, ...path].join(' › '), parts: fieldParts(f) });
  }
}

const JSON_BODY = 'content/application~1json/schema';
const bodyOf = (part: Json | undefined) => (part?.content?.['application/json'] as Json | undefined)?.schema;

/** The pointers an operation draws: itself, its auth, parameters, request and response fields, and each response. */
export function opUnits(doc: Json, o: Operation): Units {
  const label = `${o.method} ${o.path}`;
  const out: Units = new Map([[o.ptr, { label, parts: parts({ summary: o.op.summary, description: o.op.description, operationId: o.op.operationId, tags: o.op.tags, deprecated: o.op.deprecated }) }]]);
  if (o.op.security) out.set(`${o.ptr}/security`, { label: `${label} › auth`, parts: parts({ auth: o.op.security }) });
  addRows(out, paramFields(doc, o), [label]);
  const schema = (raw: Json | undefined, at: string, owner: string) => {
    const r = resolve(doc, raw, at);
    if (r) addRows(out, fields(doc, r.schema, r.ptr), [label, owner]);
  };
  schema(bodyOf(o.op.requestBody), `${o.ptr}/requestBody/${JSON_BODY}`, 'request');
  for (const [code, res] of Object.entries((o.op.responses ?? {}) as Json)) {
    out.set(`${o.ptr}/responses/${code}`, { label: `${label} › ${code}`, parts: parts({ description: res.description, content: Object.keys(res.content ?? {}) }) });
    schema(bodyOf(res), `${o.ptr}/responses/${code}/${JSON_BODY}`, code);
  }
  return out;
}

const declParts = (d: Decl) => parts({ kind: d.kind, exported: d.exported ? 'exported' : 'not exported', generics: d.generics, doc: d.doc, type: d.text });

function documentUnits(spec: ContractSpec, model?: Model): Units {
  const doc = spec.document as Json;
  if (spec.kind === 'rest') return new Map(operations(doc).flatMap((o) => [...opUnits(doc, o)]));
  const out: Units = new Map();
  if (spec.kind === 'json-schema') addRows(out, fields(doc, doc, ''), []);
  for (const d of model?.decls ?? []) {
    out.set(`/${d.name}`, { label: d.name, parts: declParts(d) });
    addRows(out, d.rows, [d.name]);
  }
  return out;
}

/** What a version draws, by pointer: the title and purpose, then the kind's elements. A TypeScript spec needs its parsed model. */
export function units(spec: ContractSpec, model?: Model): Units {
  const { purpose, nonGoals, audience, consumers, guarantees } = spec.intent;
  return new Map([
    ['#title', { label: 'Title', parts: parts({ title: spec.title }) }],
    ['#purpose', { label: 'What it does', parts: parts({ purpose, 'non-goals': nonGoals, audience, consumers, guarantees }) }],
    ...documentUnits(spec, model),
  ]);
}

const whole = (ptr: string, u: Unit, mark: Change['mark']): Change =>
  ({ ptr, mark, crumb: u.label, rows: Object.entries(u.parts).map(([name, t]) => (mark === 'removed' ? { name, was: t, now: '' } : { name, was: '', now: t })) });

// One change per part that differs, its name the breadcrumb's last step.
const partChanges = (ptr: string, was: Unit, now: Unit): Change[] =>
  [...new Set([...Object.keys(was.parts), ...Object.keys(now.parts)])].filter((k) => was.parts[k] !== now.parts[k])
    .map((k) => ({ ptr, mark: 'changed', crumb: `${now.label} › ${k}`, rows: [{ was: was.parts[k] ?? '', now: now.parts[k] ?? '' }] }));

/** What changed from `was` to `now`: each element of `now` that is new or differs, marked by pointer, and every change in the order `now` draws, removals last. */
export function diffUnits(was: Units, now: Units): Diff {
  const marks = new Map<string, Mark>();
  const changes: Change[] = [];
  for (const [ptr, u] of now) {
    const before = was.get(ptr);
    const own = before ? partChanges(ptr, before, u) : [whole(ptr, u, 'added')];
    if (own.length) marks.set(ptr, before ? 'changed' : 'added');
    changes.push(...own);
  }
  for (const [ptr, u] of was) if (!now.has(ptr)) changes.push(whole(ptr, u, 'removed'));
  return { marks, changes };
}

/** The marks of the comparison on screen; outside one nothing is marked. */
export const DiffContext = createContext<Map<string, Mark> | undefined>(undefined);
export const useMark = (ptr: string) => useContext(DiffContext)?.get(ptr);
/** Whether any of the pointers is marked, for an element that stands for several (a route in the list). */
export const useAnyMark = (ptrs: Iterable<string>) => {
  const marks = useContext(DiffContext);
  return !!marks && [...ptrs].some((p) => marks.has(p));
};
