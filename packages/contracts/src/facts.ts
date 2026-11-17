// No node: imports, as spec.ts, which imports this, has none.
import type { Change, Intent } from './spec.ts';

/** One entry of intent.guarantees or intent.nonGoals: a stored string, or one checkable fact with a stable id. */
export type Fact = string | { id: string; text: string };

export const FACT_ID = /^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$/;
export const FACT_LIMIT = 160;
const LISTS = ['guarantees', 'nonGoals'] as const;

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
/** An entry's text: the string as-is, or the object's text. */
export const factText = (f: unknown): string => (typeof f === 'string' ? f : isObj(f) && typeof f.text === 'string' ? f.text : '');
const idOf = (f: unknown) => (isObj(f) && typeof f.id === 'string' ? f.id : undefined);
// JSON Schema's maxLength counts code points; .length counts UTF-16 units.
const points = (s: string) => [...s].length;

type Entry = { list: string; at: string; raw: unknown; id?: string; text: string };
const entriesOf = (intent: Partial<Record<string, unknown>> | undefined): Entry[] =>
  LISTS.flatMap((list) => (Array.isArray(intent?.[list]) ? (intent[list] as unknown[]) : []).map((raw, i) => ({ list, at: `${list}[${i}]`, raw, id: idOf(raw), text: factText(raw) })));

// The message template contract.spec v7 records in factEntry's $comment.
const fail = (at: string, reason: string) => `facts are not decomposed: ${at}: ${reason}`;
const NO_ID = 'no id — give it one';
const reasons = {
  over: (n: number) => `over ${FACT_LIMIT} — the text is ${n} characters, so split it into one fact per entry`,
  bad: (id: string) => `bad id — '${id}' is not lowercase dotted or dashed, so rename it`,
  duplicate: (id: string) => `duplicate id — '${id}' is used more than once, so give each entry its own id`,
  renamed: (id: string) => `renamed id — the text matches the baseline's id '${id}', so keep that id`,
  unnamed: (id: string) => `id named in no changes row's facts — '${id}' was added with new text, changed or removed, so name it in the row that touches the list`,
  untouched: (id: string) => `facts id the revision did not touch — '${id}' was named but was neither added, changed nor removed, and is not an id given to unchanged text, so remove it from facts`,
};

function shapeOf(e: Entry, needId: boolean): string[] {
  if (typeof e.raw === 'string') return !e.raw ? [`intent.${e.at} must be a non-empty string or an {id, text} fact`] : needId ? [fail(e.at, NO_ID)] : [];
  if (!isObj(e.raw)) return [`intent.${e.at} must be a non-empty string or an {id, text} fact`];
  const out = e.id === undefined ? [fail(e.at, NO_ID)] : FACT_ID.test(e.id) ? [] : [fail(e.at, reasons.bad(e.id))];
  if (typeof e.raw.text !== 'string' || !e.raw.text) out.push(`intent.${e.at}.text must be a non-empty string`);
  else if (points(e.raw.text) > FACT_LIMIT) out.push(fail(e.at, reasons.over(points(e.raw.text))));
  return out;
}

/** What needs no baseline: each entry's shape, ids unique across both lists, and with `needIds` every entry id'd. */
export function factShapeProblems(intent: Partial<Intent> | undefined, needIds: boolean): string[] {
  const seen = new Set<string>();
  return entriesOf(intent).flatMap((e) => {
    const out = shapeOf(e, needIds);
    if (e.id !== undefined && seen.has(e.id)) out.push(fail(e.at, reasons.duplicate(e.id)));
    if (e.id !== undefined) seen.add(e.id);
    return out;
  });
}

type Touched = { required: Map<string, string>; optional: Set<string>; out: string[] };

// Baseline entries match by id first, then by text within the same list; each baseline entry matches once.
function touched(now: Entry[], was: Entry[]): Touched {
  const t: Touched = { required: new Map(), optional: new Set(), out: [] };
  const used = new Set<Entry>();
  const byId = new Map(was.filter((b) => b.id !== undefined).map((b) => [b.id!, b] as const));
  const rest = now.filter((e) => {
    const b = e.id === undefined ? undefined : byId.get(e.id);
    if (!b || used.has(b)) return true;
    used.add(b);
    if (b.text !== e.text || b.list !== e.list) t.required.set(e.id!, e.at);
    return false;
  });
  for (const e of rest) {
    const b = was.find((x) => !used.has(x) && x.list === e.list && x.text === e.text);
    if (b) used.add(b);
    if (!b && e.id !== undefined) t.required.set(e.id, e.at);
    else if (!b) t.out.push(...(typeof e.raw === 'string' ? [fail(e.at, NO_ID)] : []));
    else if (b.id !== undefined && e.id !== b.id) t.out.push(fail(e.at, reasons.renamed(b.id)));
    else if (b.id === undefined && e.id !== undefined) t.optional.add(e.id);
  }
  const ids = new Set(now.map((e) => e.id));
  for (const b of was) if (b.id !== undefined && !used.has(b) && !ids.has(b.id)) t.required.set(b.id, `(id '${b.id}')`);
  return t;
}

/** The facts_decomposed gate against the approved spec; with none, every entry must be an id'd fact. Messages follow the v7 template. */
export function factProblems(spec: { intent?: Partial<Intent>; changes?: Change[] }, approved?: { intent?: Partial<Intent> }): string[] {
  const shape = factShapeProblems(spec.intent, !approved);
  if (!approved) return shape;
  const t = touched(entriesOf(spec.intent), entriesOf(approved.intent));
  const rows = (Array.isArray(spec.changes) ? spec.changes : []).map((c) => (isObj(c) && Array.isArray(c.facts) ? c.facts.map(String) : []));
  const named = new Set(rows.flat());
  const unnamed = [...t.required].filter(([id]) => !named.has(id)).map(([id, at]) => fail(at, reasons.unnamed(id)));
  const stray = rows.flatMap((ids, r) => ids.filter((id) => !t.required.has(id) && !t.optional.has(id)).map((id) => fail(`changes[${r}].facts`, reasons.untouched(id))));
  return [...shape, ...t.out, ...unnamed, ...stray];
}
