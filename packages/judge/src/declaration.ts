import type { JsonObject } from '@earendil-works/pi-durable';
import { holds, itemIndependent, joined, keep, runSplitter, scoped, select } from './library.ts';
import type { Evidence, Unit } from './rubric.ts';

type JsonValue = JsonObject[string];

/** Named texts the declaration's splitters and selectors read, gathered once per sealed version. */
export type Inputs = Record<string, string>;

/** One piece of one input, as a splitter produces it. */
export type Chunk = {
  id: string;
  /** Stable key the context selectors can match on (a file path, an item's key). */
  key?: string;
  text: string;
  /** Strategy-specific data that travels with the item (for instance, the span the LLM splitter quoted for it).
   *  For a grouped fold: the item's group key under the fold's `groupBy`, and its evidence, which goes on the unit's `shared`. */
  meta?: Record<string, unknown>;
};

/** A filter on an item's fields; the third form compares a field of the item against a field of the context joined under `as`. */
export type Where =
  | { path: string; op: 'truthy' | 'falsy' }
  | { path: string; op: 'eq' | 'ne'; value: unknown }
  | { path: string; op: 'eq' | 'ne'; as: string; field: string };

/** The shape a deterministic splitter expects its input to have; when it does not, the declared `fallback` runs instead. */
export type SplitExpect = { min?: number; max?: number; maxChars?: number };

/** How a scope splits one or more inputs into the items a check is fanned out over. */
export type SplitDecl = {
  name: string;
  from: string | string[];
  sections?: string[];
  skip?: string[];
  strip?: string;
  match?: string;
  splitOn?: string;
  minWords?: number;
  max?: number;
  names?: string[];
  path?: string;
  filter?: 'added';
  where?: Where[];
  pick?: string[];
  of?: SplitDecl;
  extract?: string;
  compare?: string;
  minShared?: number;
  top?: number;
  instruction?: string;
  expect?: SplitExpect;
  fallback?: SplitDecl;
  sweep?: SplitDecl;
};

/** A context selector: named extra state added to every item's request. */
export type SelectorDecl =
  | { name: 'whole'; from: string; maxChars?: number }
  | { name: 'sections'; from: string; names?: string[]; maxChars?: number }
  | { name: 'items'; from: string; sections?: string[]; skip?: string[]; strip?: string; maxChars?: number }
  | { name: 'json'; from: string; pick?: string[]; pointer?: string | { field: string } }
  | { name: 'related'; chunker: SplitDecl; match: string | string[]; top?: number; min?: number; maxChars?: number; require?: boolean }
  | { name: 'join'; from: string; by?: string }
  | ({ name: string } & Record<string, unknown>);

/** A named set of items a check fans out over: how they are split, and the named context shown with each. */
export type Scope = { split: SplitDecl; piece?: string; context?: Record<string, SelectorDecl> };

/** The question asked of one item, shown with the scope's named context; a bool question's `pass` names the answer that means the item holds (`['yes']` by default). */
export type Question = { type: 'bool'; instructions: string; pass?: ('yes' | 'no')[] } | { type: 'choice'; instructions: string; options: Record<string, string> };

/** The fan-in: how the per-item answers fold into the check's result; a `grouped` fold folds each group's items with `inner`, then the groups' results with `outer`. */
export type AggregateDecl = {
  name: 'all' | 'any' | 'share' | 'atLeast' | 'coverageUnion' | 'grouped';
  /** `atLeast`, or `grouped` with `outer: 'atLeast'`: the number of items (groups) that must hold. */
  k?: number;
  /** `share` and `coverageUnion`, or `grouped` with `outer: 'share'`: the share of items (groups) that must hold. */
  min?: number;
  by?: 'related' | 'answer' | 'either';
  /** `grouped` only, required: the field of the item's `meta` that holds its group key. */
  groupBy?: string;
  /** `grouped` only, required: the fold per group; a decided group's remaining items are not asked. */
  inner?: 'any' | 'all';
  /** `grouped` only, required: the fold over the groups' three-valued results. */
  outer?: 'share' | 'all' | 'any' | 'atLeast';
  pass?: string[];
};

const GROUPING = ['groupBy', 'inner', 'outer'] as const;
/** The `groupBy` of the grouped checks on a scope, which puts its items' meta on their units; undefined when none is grouped. */
const groupByOf = (decl: Declaration, scope: string) => Object.values(decl.checks).find((c) => c.scope === scope && c.aggregate.name === 'grouped')?.aggregate.groupBy;

/** The decisionModel value under which the decision model may pass an item but not fail it; the judge model decides and explains a failing one. */
export const PASS_ONLY = 'pass-only' as const;

/** How one model check is judged: the scope it fans out over, the question asked per item, and the fold. */
export type CheckDecl = {
  scope: string;
  question: Question;
  aggregate: AggregateDecl;
  /** false: the check's questions skip the decision model and go to the judge model, packed with the group's other escalations.
   *  PASS_ONLY: the decision model may pass an item but never fail one; a failing answer escalates like an unsure one, and the judge model's answer and reason stand. */
  decisionModel?: false | typeof PASS_ONLY;
};

/** The named scopes, and, keyed by rubric check id, how each model check is judged. */
export type Declaration = { scopes: Record<string, Scope>; checks: Record<string, CheckDecl> };

/** Which splitter produced a scope's items and why a fallback ran; `failed` when the model splitter gave no usable split. */
export type Splitting = { splitter: string; reason?: string; failed?: string };

/** Asks the splitting model for the items of free-form text, each with the span it quotes; undefined when it gave no usable answer. */
export type ModelSplit = (text: string, o: SplitDecl, role: string) => Promise<Array<{ item: string; span: string }> | undefined>;

class SplitFailed extends Error {}

const flat = (s: string) => s.replace(/\s+/g, ' ').trim();
const spanRe = (span: string) => new RegExp(flat(span).replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/ /g, '\\s+'));
const REST = '\u0000rest';

// Spans the model made up are dropped; the sweep splits whatever text no kept span covers, so a missed item is still judged.
async function llmSplit(inputs: Inputs, o: SplitDecl, model: ModelSplit | undefined, role: string): Promise<Chunk[]> {
  const text = scoped(inputs, o);
  const found = model && (await model(text, o, role));
  if (!found) throw new SplitFailed('the splitting model gave no usable split');
  const kept = found.filter((x) => flat(x.span) && flat(text).includes(flat(x.span)));
  const chunks = keep(kept.map((x, n) => ({ id: `i${n + 1}`, text: x.item, meta: { span: x.span } })), o);
  if (!o.sweep) return chunks;
  const rest = kept.reduce((t, x) => t.replace(spanRe(x.span), ' '), text);
  const extra = runSplitter({ ...inputs, [REST]: rest }, { ...o.sweep, from: REST, sections: undefined, skip: undefined });
  return [...chunks, ...extra.map((c) => ({ ...c, id: `x${c.id}` }))];
}

const runSplit = (inputs: Inputs, o: SplitDecl, model: ModelSplit | undefined, role: string) =>
  o.name === 'llm' ? llmSplit(inputs, o, model, role) : Promise.resolve(runSplitter(inputs, o));

/** Why a deterministic split does not have the shape its input was expected to give, if it does not. */
export function shapeProblem(pieces: Chunk[], e: SplitExpect | undefined): string | undefined {
  if (e?.min !== undefined && pieces.length < e.min) return `fewer than ${e.min} items`;
  if (e?.max !== undefined && pieces.length > e.max) return `more than ${e.max} items`;
  if (e?.maxChars !== undefined && pieces.some((c) => c.text.length > e.maxChars!)) return `an item over ${e.maxChars} characters`;
  return undefined;
}

async function split(inputs: Inputs, o: SplitDecl, model: ModelSplit | undefined, scope: string): Promise<{ chunks: Chunk[] } & Splitting> {
  const chunks = await runSplit(inputs, o, model, scope);
  const reason = o.fallback ? shapeProblem(chunks, o.expect) : undefined;
  if (!reason) return { chunks, splitter: o.name };
  return { chunks: await runSplit(inputs, o.fallback!, model, `${scope}:fallback`), splitter: `${o.name} -> ${o.fallback!.name}`, reason };
}

// JSON text goes to the model as JSON, not as an escaped string; any other text goes as it is.
function asValue(text: string): JsonValue {
  if (!/^\s*[[{"]/.test(text)) return text;
  try {
    return JSON.parse(text) as JsonValue;
  } catch {
    return text;
  }
}

type Built = { unit: Unit; group?: string } | undefined;

// A grouped scope's item carries its group key and evidence in its meta: the key groups it, the rest is its shared context.
function grouping(name: string, chunk: Chunk, groupBy: string | undefined): { group?: string; rest: JsonObject } {
  if (!groupBy) return { rest: {} };
  const { [groupBy]: group, ...rest } = chunk.meta ?? {};
  if (typeof group !== 'string' && typeof group !== 'number') throw new Error(`scope ${name}: item ${chunk.id} has no string or number group key ${groupBy} in its meta`);
  return { group: String(group), rest: rest as JsonObject };
}

// Whether every cross-input `where` holds for the item against its joined record.
function crossHolds(inputs: Inputs, name: string, scope: Scope, chunk: Chunk): boolean {
  return (scope.split.where ?? []).filter((w) => 'as' in w).every((w) => {
    const sel = scope.context?.[(w as { as: string }).as];
    if (sel?.name !== 'join') throw new Error(`scope ${name}: a where compares against "${(w as { as: string }).as}", which is not a join in its context`);
    return holds(w, chunk.meta ?? {}, joined(inputs, chunk, sel as Extract<SelectorDecl, { name: 'join' }>));
  });
}

// An item is dropped when a selector finds nothing it requires, or a cross-input `where` does not hold for it.
function unitOf(inputs: Inputs, name: string, scope: Scope, chunk: Chunk, splitOther: (o: SplitDecl) => Chunk[], groupBy?: string): Built {
  if (!crossHolds(inputs, name, scope, chunk)) return undefined;
  const state: JsonObject = { [scope.piece ?? 'piece']: asValue(chunk.text) };
  const { group, rest: shared } = grouping(name, chunk, groupBy);
  const related: Record<string, number> = {};
  for (const [as, sel] of Object.entries(scope.context ?? {})) {
    const got = select(inputs, chunk, sel, splitOther);
    if (got.value === undefined) return undefined;
    // A selector that reports matches with an empty value adds no field: an empty field reads to the model as a missing thing.
    if (got.value !== '' || got.matches === undefined) (itemIndependent(sel) ? shared : state)[as] = asValue(got.value);
    if (got.matches !== undefined) related[as] = got.matches;
  }
  // A declared item is named by its scope, so units of two scopes never share an id; a whole-input item is named once.
  const id = chunk.id === name ? name : `${name} ${chunk.id}`;
  return { unit: { id, state, ...(Object.keys(related).length ? { related } : {}), ...(Object.keys(shared).length ? { shared } : {}) }, ...(group !== undefined && { group }) };
}

/** `groups`: for each scope a grouped check reads, every unit's group key by unit id. */
export type BuiltEvidence = { evidence: Evidence; splits: Record<string, Splitting>; groups?: Record<string, Record<string, string>> };

/** Builds the units of every scope (or those `only` keeps) from the inputs: one unit per item, its state the piece and item context, its shared the context every item sees. */
export async function buildEvidence(decl: Declaration, inputs: Inputs, model?: ModelSplit, only: (scope: Scope) => boolean = () => true): Promise<BuiltEvidence> {
  const evidence: Evidence = {};
  const splits: Record<string, Splitting> = {};
  const groups: Record<string, Record<string, string>> = {};
  // A related selector's candidates are the same for every item, so each chunker runs once per build.
  const cache = new Map<string, Chunk[]>();
  const splitOther = (o: SplitDecl) => cache.get(JSON.stringify(o)) ?? cache.set(JSON.stringify(o), runSplitter(inputs, o)).get(JSON.stringify(o))!;
  for (const [name, scope] of Object.entries(decl.scopes).filter(([, s]) => only(s))) {
    try {
      const { chunks, ...how } = await split(inputs, scope.split, model, name);
      const groupBy = groupByOf(decl, name);
      const built = chunks.map((c) => unitOf(inputs, name, scope, c, splitOther, groupBy)).filter((b) => b !== undefined);
      evidence[name] = built.map((b) => b.unit);
      if (groupBy) groups[name] = Object.fromEntries(built.map((b) => [b.unit.id, b.group!]));
      splits[name] = how;
    } catch (err) {
      if (!(err instanceof SplitFailed)) throw err;
      evidence[name] = [];
      splits[name] = { splitter: scope.split.name, failed: err.message };
    }
  }
  return { evidence, splits, ...(Object.keys(groups).length ? { groups } : {}) };
}

/** A declaration names a scope for every model check of the rubric and nothing else; a gap is an extension error. */
export function checkDeclaration(decl: Declaration, modelChecks: string[]): void {
  const missing = modelChecks.filter((id) => !decl.checks[id]);
  const extra = Object.keys(decl.checks).filter((id) => !modelChecks.includes(id));
  const unscoped = Object.entries(decl.checks).filter(([, c]) => !decl.scopes[c.scope]).map(([id, c]) => `${id} (${c.scope})`);
  const routed = Object.entries(decl.checks).filter(([, c]) => c.decisionModel !== undefined && c.decisionModel !== false && c.decisionModel !== PASS_ONLY).map(([id]) => id);
  const misgrouped = Object.entries(decl.checks).filter(([, c]) => (c.aggregate.name === 'grouped' ? !GROUPING.every((f) => c.aggregate[f] !== undefined) : GROUPING.some((f) => c.aggregate[f] !== undefined))).map(([id]) => id);
  // A scope's units carry one group key, so its grouped checks must agree on where it is.
  const torn = Object.entries(decl.checks).filter(([, c]) => c.aggregate.name === 'grouped' && c.aggregate.groupBy !== groupByOf(decl, c.scope)).map(([id]) => id);
  const problems = [missing.length && `no declaration for ${missing.join(', ')}`, extra.length && `declared checks the rubric lacks: ${extra.join(', ')}`, unscoped.length && `checks on undeclared scopes: ${unscoped.join(', ')}`, routed.length && `decisionModel may only be false or ${PASS_ONLY}: ${routed.join(', ')}`, misgrouped.length && `a grouped fold needs groupBy, inner and outer, and only a grouped fold takes them: ${misgrouped.join(', ')}`, torn.length && `grouped checks on one scope group by different fields: ${torn.join(', ')}`].filter(Boolean);
  if (problems.length) throw new Error(`the judge declaration does not match its rubric: ${problems.join('; ')}`);
}
