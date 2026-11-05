// The spike's strategy library. A check is a declaration over these named, parameterised strategies; nothing here knows a candidate.
// Inputs are named texts the artifact kind supplies (report, request, diff, review, ...); a chunk is one piece of one input.

export type Inputs = Record<string, string>;
export type Opts = Record<string, any>;
export type Ref = { name: string } & Opts;
export type Chunk = { id: string; key?: string; text: string; meta?: Record<string, any> };
/** What the chunk's request shows the decision model: the piece plus named context. */
export type Unit = { id: string; key?: string; state: Record<string, string>; related: Record<string, number> };

const words = (s: string) => new Set((s.toLowerCase().match(/[a-z][a-z0-9]{3,}/g) ?? []).filter((w) => !STOP.has(w)));
const STOP = new Set(['that', 'this', 'with', 'from', 'have', 'which', 'what', 'when', 'will', 'does', 'into', 'also', 'than', 'then', 'there', 'their', 'each', 'only', 'more', 'most', 'over', 'such', 'both', 'been', 'were', 'they', 'them', 'where', 'while', 'about', 'after', 'before']);

/** The text under `## name` headings (all of them when `names` is empty), without the headings. */
export function sectionText(text: string, names: string[] = []): string {
  const parts = text.split(/^## /m).slice(1).map((p) => ({ head: p.split('\n', 1)[0].trim().toLowerCase(), body: p.slice(p.indexOf('\n') + 1) }));
  const want = names.map((n) => n.toLowerCase());
  return parts.filter((p) => !want.length || want.includes(p.head)).map((p) => p.body.trim()).join('\n');
}
const from = (inputs: Inputs, o: Opts): string => (Array.isArray(o.from) ? o.from : [o.from]).map((k: string) => inputs[k] ?? '').join('\n');
/** The input without the `skip` sections (the preamble stays), or only its `sections`. */
function dropSections(text: string, skip: string[]): string {
  const [pre, ...rest] = text.split(/^## /m);
  return [pre, ...rest.filter((p) => !skip.includes(p.split('\n', 1)[0].trim().toLowerCase())).map((p) => `## ${p}`)].join('');
}
const scoped = (inputs: Inputs, o: Opts) => (o.sections ? sectionText(from(inputs, o), o.sections) : o.skip ? dropSections(from(inputs, o), o.skip) : from(inputs, o));
const strip = (s: string, o: Opts) => (o.strip ? s.replace(new RegExp(o.strip, 'g'), '') : s).replace(/[ \t]+/g, ' ').trim();
const keep = (chunks: Chunk[], o: Opts): Chunk[] => {
  const re = o.match ? new RegExp(o.match, 'i') : undefined;
  const out = chunks.filter((c) => /[\p{L}\p{N}]/u.test(c.text) && c.text.split(/\s+/).length >= (o.minWords ?? 1) && (!re || re.test(c.text)));
  return o.max ? out.slice(0, o.max) : out;
};

/** Side channel for the splitter that just ran: spans the model made up, and chunks the sweep added. */
const split = { rejected: 0, swept: 0 };

// Chunkers: split an input on its own structure.
export const chunkers: Record<string, (inputs: Inputs, o: Opts) => Chunk[]> = {
  /** `## ` sections. */
  markdownSections: (i, o) => from(i, o).split(/^## /m).slice(1).map((p) => ({ id: p.split('\n', 1)[0].trim(), text: p.slice(p.indexOf('\n') + 1).trim() })).filter((c) => !o.names || o.names.includes(c.id.toLowerCase())),
  /** Bullet and numbered items with their continuation lines. */
  listItems: (i, o) => {
    const items: string[][] = [];
    for (const line of scoped(i, o).split('\n')) {
      if (/^\s*([-*]|\d+\.)\s/.test(line)) items.push([line.replace(/^\s*([-*]|\d+\.)\s+(\[[ x]\]\s*)?/, '')]);
      else if (items.length && line.trim()) items.at(-1)!.push(line.trim());
    }
    return keep(items.map((l, n) => ({ id: `item-${n + 1}`, text: strip(l.join(' '), o) })), o);
  },
  /** Sentences, or clauses when `splitOn` is given; bullets and headings count as sentence ends. */
  sentences: (i, o) => {
    const out: string[] = [];
    for (const line of scoped(i, o).split('\n').map((l) => l.replace(/^\s*([-*]|\d+\.)\s+(\[[ x]\]\s*)?/, '').replace(/^#+\s.*$/, '').trim()).filter(Boolean)) {
      const sentences = line.split(/(?<=[.!?])\s+(?=[A-Z`"(])/);
      for (const s of sentences) out.push(...(o.splitOn ? s.split(new RegExp(o.splitOn, 'i')) : [s]));
    }
    return keep(out.map((s, n) => ({ id: `s${n + 1}`, text: strip(s.replace(/[?]+$/, ''), o) })).filter((c) => c.text), o);
  },
  /** One chunk per file of a unified diff; `filter: 'added'` keeps new files. */
  diffFiles: (i, o) => from(i, o).split(/^(?=diff --git )/m).filter((p) => p.startsWith('diff --git')).map((p) => ({ id: /^diff --git a\/(\S+)/.exec(p)![1], key: /^diff --git a\/(\S+)/.exec(p)![1], text: p, meta: { added: /^new file mode/m.test(p) } })).filter((c) => o.filter !== 'added' || c.meta!.added),
  /** One chunk per hunk, keyed by file. */
  diffHunks: (i, o) => chunkers.diffFiles(i, o).flatMap((f) => f.text.split(/^(?=@@ )/m).slice(1).map((h, n) => ({ id: `${f.id}#${n + 1}`, key: f.key, text: `${f.key} ${h}` }))),
  /** Items of a JSON array or object at a dotted path. */
  jsonItems: (i, o) => {
    const root = (o.path ? String(o.path).split('.') : []).reduce((v: any, k) => v?.[k], JSON.parse(from(i, o)));
    return (Array.isArray(root) ? root.map((v, n) => [String(v?.path ?? n), v]) : Object.entries(root ?? {})).slice(0, o.max ?? 1e9).map(([k, v]: any) => ({ id: String(k), key: String(k), text: typeof v === 'string' ? v : (v?.text ?? JSON.stringify(v)), meta: typeof v === 'object' ? v : {} }));
  },
  /** One chunk per OpenAPI operation. */
  openapiOperations: (i, o) => Object.entries<any>(JSON.parse(from(i, o)).paths ?? {}).flatMap(([p, ops]) => Object.entries<any>(ops).filter(([m]) => /^(get|put|post|delete|patch)$/.test(m)).map(([m, op]) => ({ id: `${m.toUpperCase()} ${p}`, text: `${m.toUpperCase()} ${p} ${JSON.stringify(op)}` }))).slice(0, o.max ?? 1e9),
  /** One chunk per property of a JSON Schema. */
  schemaProperties: (i, o) => Object.entries<any>(JSON.parse(from(i, o)).properties ?? {}).slice(0, o.max ?? 1e9).map(([k, v]) => ({ id: k, key: k, text: `${k}: ${JSON.stringify(v)}` })),
  /** One chunk per top-level export of a .d.ts file, with its preceding doc comment. */
  dtsExports: (i, o) => from(i, o).split(/\n(?=(?:\/\*\*[\s\S]*?\*\/\n)?export )/).filter((b) => /export /.test(b)).map((b) => ({ id: /export\s+(?:declare\s+)?(?:default\s+)?(?:\w+\s+)?(\w+)/.exec(b)?.[1] ?? 'export', text: b.trim() })),
  /** Overlapping windows of a long text, best `top` by word overlap with input `by`, in text order. */
  relevanceWindow: (i, o) => {
    const text = from(i, o);
    const size = o.size ?? 2000;
    const want = words(i[o.by] ?? '');
    const wins = Array.from({ length: Math.max(1, Math.ceil(text.length / (size / 2))) }, (_, n) => ({ id: `w${n + 1}`, text: text.slice((n * size) / 2, (n * size) / 2 + size) }));
    const score = (t: string) => [...words(t)].filter((w) => want.has(w)).length;
    return wins.sort((a, b) => score(b.text) - score(a.text)).slice(0, o.top ?? 3).sort((a, b) => a.id.localeCompare(b.id, undefined, { numeric: true }));
  },
  /** Items a model extracted from free-form input, each with the span it came from (`recorded` names an input holding that JSON); an item whose span is not in the input is dropped. `sweep` splits the text no span covers, so nothing goes unjudged. */
  llm: (i, o) => {
    const text = from(i, o);
    const flat = (t: string) => t.replace(/\s+/g, ' ');
    const found = (JSON.parse(i[o.recorded] ?? '[]') as Array<{ item: string; span: string }>).filter((x) => flat(text).includes(flat(x.span)));
    split.rejected += JSON.parse(i[o.recorded] ?? '[]').length - found.length;
    const chunks: Chunk[] = found.map((x, n) => ({ id: `i${n + 1}`, text: x.item, meta: { span: x.span } }));
    if (!o.sweep) return chunks;
    const rest = found.reduce((t, x) => t.replace(x.span, ' '), text);
    const extra = chunkers[o.sweep.name]({ ...i, _rest: rest }, { ...o.sweep, from: '_rest' });
    split.swept += extra.length;
    return [...chunks, ...extra.map((c) => ({ ...c, id: `x${c.id}` }))];
  },
  /** Pairs of passages whose extracted facts disagree (the map and compare halves of map-reduce); the reduce is an aggregator over the pairs. */
  digestPairs: (i, o) => {
    const pieces = chunkers[o.of.name](i, o.of);
    const facts = pieces.map((p) => extractors[o.extract](p.text));
    const pairs = pieces.flatMap((a, x) => pieces.slice(x + 1).map((b, y) => ({ a, b, s: comparators[o.compare](facts[x], facts[x + 1 + y], o) })));
    return pairs.filter((p) => p.s > 0).sort((p, q) => q.s - p.s).slice(0, o.top ?? 1e9).map((p) => ({ id: `${p.a.id}|${p.b.id}`, text: '', meta: { a: p.a.text, b: p.b.text } }));
  },
};

// Digest extractors: a small structured summary of a chunk, by code.
export type Fact = { cls: string; value: string };
export type Digest = { facts: Fact[]; terms: Set<string> };
const MONEY = /\$\s?\d[\d,.]*(?:\s?(?:million|billion|thousand|k|m))?/gi;
const TIME = /\b\d{1,2}(?::\d\d)?\s?(?:am|pm)\b/gi;
const DATE = /\b(?:\d{1,2}\s)?(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?(?:\s\d{1,2})?,?\s\d{4}\b|\b(?:19|20)\d\d\b/gi;
const NUM = /\b\d[\d,]*(?:\.\d+)?\b(?:\s?(?:%|percent|km|hectares|days|months|years|metres|meters|m\b))?/gi;
export const extractors: Record<string, (text: string) => Digest> = {
  /** Money, times, dates and plain quantities, each tagged with a class; the rest of the passage is its terms. */
  numbers: (text) => {
    const facts: Fact[] = [];
    let rest = text;
    for (const [cls, re] of [['money', MONEY], ['time', TIME], ['date', DATE]] as const) {
      for (const m of rest.matchAll(re)) facts.push({ cls, value: m[0].toLowerCase().replace(/[\s,]/g, '') });
      rest = rest.replace(re, ' ');
    }
    for (const m of rest.matchAll(NUM)) facts.push({ cls: 'number', value: m[0].toLowerCase().replace(/[\s,]/g, '') });
    return { facts, terms: words(rest.replace(NUM, ' ').replace(/\[s\d+\]/g, ' ')) };
  },
};

// Comparators: whether two digests may conflict. Neither fact set containing the other, with enough shared terms, is a candidate.
export const comparators: Record<string, (a: Digest, b: Digest, o: Opts) => number> = {
  /** Shared-term count when, in some fact class both have, neither value set contains the other; else 0. */
  sameTermsOtherValue: (a, b, o) => {
    const shared = [...a.terms].filter((t) => b.terms.has(t)).length;
    if (shared < (o.minShared ?? 2)) return 0;
    const val = (d: Digest, cls: string) => new Set(d.facts.filter((f) => f.cls === cls).map((f) => f.value));
    const sub = (p: Set<string>, q: Set<string>) => [...p].every((v) => q.has(v));
    const clash = [...new Set(a.facts.map((f) => f.cls))].some((cls) => {
      const [x, y] = [val(a, cls), val(b, cls)];
      return x.size > 0 && y.size > 0 && !sub(x, y) && !sub(y, x);
    });
    return clash ? shared : 0;
  },
};

// Selectors: named context added to every chunk's request.
type Selector = (inputs: Inputs, chunk: Chunk, o: Opts, state: { related: number }) => string | undefined;
const matchers: Record<string, (piece: Chunk, other: Chunk, o: Opts) => number> = {
  /** The other chunk's key (a file path) is named in the piece. */
  names: (p, r) => (r.key && (p.text.includes(r.key) || p.text.includes(r.key.split('/').pop()!)) ? 1 : 0),
  /** The piece's key is named in the other chunk. */
  cites: (p, r) => (p.key && (r.text.includes(p.key) || r.text.includes(p.key.split('/').pop()!)) ? 1 : 0),
  /** Words shared with the piece. */
  overlap: (p, r) => {
    const w = words(p.text);
    return [...words(`${r.key ?? ''} ${r.text}`)].filter((x) => w.has(x)).length;
  },
  /** Same directory and extension, sharing a name suffix with the piece's file; newest first via `meta.ts`. */
  sibling: (p, r) => {
    const dir = (s: string) => s.slice(0, s.lastIndexOf('/') + 1);
    const tail = (s: string) => { const b = s.split('/').pop()!; return b.split('.').length > 2 ? b.slice(b.indexOf('.')) : `${b.slice(0, b.lastIndexOf('.')).split(/[-_]/).pop()}${b.slice(b.lastIndexOf('.'))}`; };
    return p.key && r.key && dir(p.key) === dir(r.key) && tail(p.key) === tail(r.key) && p.key !== r.key ? 1 + (r.meta?.ts ?? 0) / 1e12 : 0;
  },
};
export const selectors: Record<string, Selector> = {
  /** An input as it is. */
  whole: (i, _c, o) => from(i, o).slice(0, o.maxChars ?? 1e9),
  /** Named sections of an input. */
  sections: (i, _c, o) => sectionText(from(i, o), o.names).slice(0, o.maxChars ?? 1e9),
  /** The items of an input, with a pattern stripped (a digest of claims without their citation marks). */
  items: (i, _c, o) => chunkers.listItems(i, o).map((c) => `- ${c.text}`).join('\n'),
  /** The chunks of another input that match this chunk (`match`), best `top`; `require` drops the chunk when none match. */
  related: (i, c, o, st) => {
    const others = chunkers[o.chunker.name](i, o.chunker);
    // Several matchers are tried in order; the first that finds anything wins.
    const hits = [o.match].flat().map((m: string) => others.map((r) => ({ r, s: matchers[m](c, r, o) })).filter((x) => x.s >= (o.min ?? 1)).sort((a, b) => b.s - a.s).slice(0, o.top ?? 3)).find((h) => h.length) ?? [];
    st.related += hits.length;
    return hits.length ? hits.map((x) => (x.r.key && !x.r.text.startsWith(x.r.key) ? `${x.r.key}: ${x.r.text}` : x.r.text)).join('\n---\n').slice(0, o.maxChars ?? 6000) : o.require ? undefined : '(none)';
  },
};

/** `expect` is the shape a deterministic splitter needs from its input; when it is not met, `fallback` (a model splitter) runs instead. */
export type Split = Ref & { expect?: { min?: number; max?: number; maxChars?: number }; fallback?: Ref };
export type Scope = { split: Split; context?: Record<string, Ref>; piece?: string };
export type Splitting = { units: Unit[]; splitter: string; reason?: string; rejected: number; swept: number };
export type Question = { type: 'bool' | 'choice'; instructions: string; options?: Record<string, string> };
export type Aggregate = { name: string; pass?: string[]; min?: number; k?: number; by?: string };
/** `wire` is the id the question is sent under; variants sharing a question share it, so one Clef answer serves them. */
export type Check = { wire?: string; scope: string; question: Question; aggregate: Aggregate; flagWhen: boolean };
export type Declaration = { scopes: Record<string, Scope>; checks: Record<string, Check> };

/** The chunks of a scope with their context, ready to send. */
export function unitsOf(scope: Scope, inputs: Inputs): Splitting {
  split.rejected = split.swept = 0;
  let pieces = chunkers[scope.split.name](inputs, scope.split);
  let splitter = scope.split.name;
  let reason: string | undefined;
  const e = scope.split.expect;
  if (e && scope.split.fallback) {
    reason = (e.min !== undefined && pieces.length < e.min) ? `fewer than ${e.min} items` : (e.max !== undefined && pieces.length > e.max) ? `more than ${e.max} items` : (e.maxChars !== undefined && pieces.some((c) => c.text.length > e.maxChars!)) ? `an item over ${e.maxChars} characters` : undefined;
    if (reason) {
      split.rejected = split.swept = 0;
      pieces = chunkers[scope.split.fallback.name](inputs, scope.split.fallback);
      splitter = `${scope.split.name} -> ${scope.split.fallback.name}`;
    }
  }
  const units: Unit[] = [];
  for (const c of pieces) {
    const state: Record<string, string> = scope.split.name === 'digestPairs' ? { first: c.meta!.a, second: c.meta!.b } : { [scope.piece ?? 'piece']: c.text };
    const st = { related: 0 };
    const related: Record<string, number> = {};
    let dropped = false;
    for (const [as, ref] of Object.entries(scope.context ?? {})) {
      st.related = 0;
      const v = selectors[ref.name](inputs, c, ref, st);
      if (v === undefined) dropped = true;
      else state[as] = v;
      related[as] = st.related;
    }
    if (!dropped) units.push({ id: c.id, key: c.key, state, related });
  }
  return { units, splitter, reason, rejected: split.rejected, swept: split.swept };
}

// Aggregators: fold chunk answers by a declared rule. Each answer is true, false or unknown (neither side reaches the act threshold).
export const ACT = 0.8;
export type Answer = { p?: number; probs?: Record<string, number> };
export type Tri = boolean | undefined;
const mass = (a: Answer | undefined, ag: Aggregate): number | undefined => (ag.pass ? (a?.probs ? ag.pass.reduce((s, k) => s + (a.probs![k] ?? 0), 0) : undefined) : a?.p);
export const tri = (a: Answer | undefined, ag: Aggregate): Tri => {
  const m = mass(a, ag);
  return m === undefined ? undefined : m >= ACT ? true : 1 - m >= ACT ? false : undefined;
};
const guess = (a: Answer | undefined, ag: Aggregate) => (mass(a, ag) ?? 0) > 0.5;

export type Folded = { value: Tri; guess: boolean; n: number; true: number; unknown: number };
type Rule = (t: number, u: number, n: number, ag: Aggregate) => Tri;
const bounds = (lo: number, hi: number, need: number): Tri => (lo >= need ? true : hi < need ? false : undefined);
const rules: Record<string, Rule> = {
  all: (t, u, n) => bounds(t, t + u, n),
  any: (t, u) => bounds(t, t + u, 1),
  share: (t, u, n, ag) => (n ? bounds(t / n, (t + u) / n, ag.min ?? 1) : true),
  atLeast: (t, u, _n, ag) => bounds(t, t + u, ag.k ?? 1),
};
/** `coverageUnion`: a chunk counts as covered when code found related material (`by: 'related'`), or the model says so (`by: 'answer'`), or either (`by: 'either'`); the share covered must reach `min`. */
export function fold(ag: Aggregate, items: Array<{ answer?: Answer; related?: number }>): Folded {
  const cover = ag.name === 'coverageUnion';
  const vals = items.map((x): Tri => (cover && ag.by !== 'answer' && x.related ? true : tri(x.answer, ag)));
  const guesses = items.map((x) => (cover && ag.by !== 'answer' && x.related ? true : guess(x.answer, ag)));
  const rule = rules[cover ? 'share' : ag.name];
  const t = vals.filter((v) => v === true).length;
  const u = vals.filter((v) => v === undefined).length;
  const g = guesses.filter(Boolean).length;
  return { value: rule(t, u, items.length, ag), guess: rule(g, 0, items.length, ag) as boolean, n: items.length, true: t, unknown: u };
}
