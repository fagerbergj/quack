import type { JsonObject } from '@earendil-works/pi-durable';
import { holds, itemIndependent, joined, runSplitter, select } from './library.ts';
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
  /** Strategy-specific data that travels with the item.
   *  For a grouped fold: the item's group key under the fold's `groupBy`, and its evidence, which goes on the unit's `shared`. */
  meta?: Record<string, unknown>;
};

/** A filter on an item's fields; the third form compares a field of the item against a field of the context joined under `as`. */
export type Where =
  | { path: string; op: 'truthy' | 'falsy' }
  | { path: string; op: 'eq' | 'ne'; value: unknown }
  | { path: string; op: 'eq' | 'ne'; as: string; field: string };

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
  where?: Where[];
  pick?: string[];
  of?: SplitDecl;
  extract?: string;
  compare?: string;
  minShared?: number;
  top?: number;
};

/** A context selector: named extra state added to every item's request. */
export type SelectorDecl =
  | { name: 'whole'; from: string; maxChars?: number }
  | { name: 'sections'; from: string; names?: string[]; maxChars?: number }
  | { name: 'items'; from: string; sections?: string[]; skip?: string[]; strip?: string; maxChars?: number }
  | { name: 'json'; from: string; pick?: string[]; pointer?: string | { field: string } }
  | { name: 'join'; from: string; by?: string }
  | ({ name: string } & Record<string, unknown>);

/** A named set of items a check fans out over: how they are split, and the named context shown with each. */
export type Scope = { split: SplitDecl; piece?: string; context?: Record<string, SelectorDecl> };

/** The question asked of one item, shown with the scope's named context; a bool question's `pass` names the answer that means the item holds (`['yes']` by default). */
export type Question = { type: 'bool'; instructions: string; pass?: ('yes' | 'no')[] } | { type: 'choice'; instructions: string; options: Record<string, string> };

/** The fan-in: how the per-item answers fold into the check's result; a `grouped` fold folds each group's items with `inner`, then the groups' results with `outer`. */
export type AggregateDecl = {
  name: 'all' | 'any' | 'share' | 'grouped';
  /** `share`, or `grouped` with `outer: 'share'`: the share of items (groups) that must hold. */
  min?: number;
  by?: 'related' | 'answer' | 'either';
  /** `grouped` only, required: the field of the item's `meta` that holds its group key. */
  groupBy?: string;
  /** `grouped` only, required: the fold per group; a decided group's remaining items are not asked. */
  inner?: 'any' | 'all';
  /** `grouped` only, required: the fold over the groups' three-valued results. */
  outer?: 'share' | 'all' | 'any';
  /** `grouped` with `inner: 'any'` only: a failing group's note when no failing item gave a reason; `{n}` is how many items it had. */
  none?: string;
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

/** Which splitter produced a scope's items. */
export type Splitting = { splitter: string };

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

// An item is dropped when a selector returns undefined, or a cross-input `where` does not hold for it.
function unitOf(inputs: Inputs, name: string, scope: Scope, chunk: Chunk, groupBy?: string): Built {
  if (!crossHolds(inputs, name, scope, chunk)) return undefined;
  const state: JsonObject = { [scope.piece ?? 'piece']: asValue(chunk.text) };
  const { group, rest: shared } = grouping(name, chunk, groupBy);
  const related: Record<string, number> = {};
  for (const [as, sel] of Object.entries(scope.context ?? {})) {
    const got = select(inputs, chunk, sel);
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

/** Builds the units of every scope from the inputs: one unit per item, its state the piece and item context, its shared the context every item sees. */
export function buildEvidence(decl: Declaration, inputs: Inputs): BuiltEvidence {
  const evidence: Evidence = {};
  const splits: Record<string, Splitting> = {};
  const groups: Record<string, Record<string, string>> = {};
  for (const [name, scope] of Object.entries(decl.scopes)) {
    const chunks = runSplitter(inputs, scope.split);
    const groupBy = groupByOf(decl, name);
    const built = chunks.map((c) => unitOf(inputs, name, scope, c, groupBy)).filter((b) => b !== undefined);
    evidence[name] = built.map((b) => b.unit);
    if (groupBy) groups[name] = Object.fromEntries(built.map((b) => [b.unit.id, b.group!]));
    splits[name] = { splitter: scope.split.name };
  }
  return { evidence, splits, ...(Object.keys(groups).length ? { groups } : {}) };
}

/** A declaration names a scope for every model check of the rubric and nothing else; a gap is an extension error. */
export function checkDeclaration(decl: Declaration, modelChecks: string[]): void {
  const missing = modelChecks.filter((id) => !decl.checks[id]);
  const extra = Object.keys(decl.checks).filter((id) => !modelChecks.includes(id));
  const unscoped = Object.entries(decl.checks).filter(([, c]) => !decl.scopes[c.scope]).map(([id, c]) => `${id} (${c.scope})`);
  const routed = Object.entries(decl.checks).filter(([, c]) => c.decisionModel !== undefined && c.decisionModel !== false && c.decisionModel !== PASS_ONLY).map(([id]) => id);
  const misgrouped = Object.entries(decl.checks).filter(([, c]) => (c.aggregate.name === 'grouped' ? !GROUPING.every((f) => c.aggregate[f] !== undefined) : GROUPING.some((f) => c.aggregate[f] !== undefined)) || (c.aggregate.none !== undefined && c.aggregate.inner !== 'any')).map(([id]) => id);
  // A scope's units carry one group key, so its grouped checks must agree on where it is.
  const torn = Object.entries(decl.checks).filter(([, c]) => c.aggregate.name === 'grouped' && c.aggregate.groupBy !== groupByOf(decl, c.scope)).map(([id]) => id);
  const problems = [missing.length && `no declaration for ${missing.join(', ')}`, extra.length && `declared checks the rubric lacks: ${extra.join(', ')}`, unscoped.length && `checks on undeclared scopes: ${unscoped.join(', ')}`, routed.length && `decisionModel may only be false or ${PASS_ONLY}: ${routed.join(', ')}`, misgrouped.length && `a grouped fold needs groupBy, inner and outer, only a grouped fold takes them, and only an inner any takes none: ${misgrouped.join(', ')}`, torn.length && `grouped checks on one scope group by different fields: ${torn.join(', ')}`].filter(Boolean);
  if (problems.length) throw new Error(`the judge declaration does not match its rubric: ${problems.join('; ')}`);
}
