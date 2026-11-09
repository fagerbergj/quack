// The judge's shared strategy library: splitters, context selectors, matchers and the map-reduce digest strategies.
// Every option a strategy reads is declared in declaration.ts; nothing here knows an artifact kind.
import type { Chunk, Inputs, SelectorDecl, SplitDecl, Where } from './declaration.ts';

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === 'object' && v !== null && !Array.isArray(v);

/** The named input's text; a declaration that reads an input the extension did not provide is a declaration error, not an empty item. */
export function input(inputs: Inputs, name: string): string {
  if (!Object.hasOwn(inputs, name)) throw new Error(`the declaration reads input "${name}", which the extension's inputs did not provide`);
  return inputs[name];
}
const from = (inputs: Inputs, f: string | string[]) => [f].flat().map((k) => input(inputs, k)).join('\n');

/** Value at a dotted path (array indexes allowed). */
export const at = (v: unknown, path?: string): unknown => (path ? path.split('.').reduce<unknown>((x, k) => (isObj(x) || Array.isArray(x) ? (x as Obj)[k] : undefined), v) : v);

const head = (part: string) => part.split('\n', 1)[0].trim();
const body = (part: string) => part.slice(part.indexOf('\n') + 1);

/** The text under `## name` headings (all of them when `names` is empty), without the headings. */
export function sectionText(text: string, names: string[] = []): string {
  const want = names.map((n) => n.toLowerCase());
  return text.split(/^## /m).slice(1).filter((p) => !want.length || want.includes(head(p).toLowerCase())).map((p) => body(p).trim()).join('\n');
}

/** The text without the `skip` sections; the preamble stays. */
function dropSections(text: string, skip: string[]): string {
  const [pre, ...rest] = text.split(/^## /m);
  const drop = skip.map((s) => s.toLowerCase());
  return [pre, ...rest.filter((p) => !drop.includes(head(p).toLowerCase())).map((p) => `## ${p}`)].join('');
}

/** A text splitter's input: the named inputs, cut to `sections` or without `skip`. */
export function scoped(inputs: Inputs, o: { from: string | string[]; sections?: string[]; skip?: string[] }): string {
  const text = from(inputs, o.from);
  return o.sections ? sectionText(text, o.sections) : o.skip ? dropSections(text, o.skip) : text;
}

const BULLET = /^\s*([-*]|\d+\.)\s+(\[[ x]\]\s*)?/;

function listItems(text: string): Chunk[] {
  const items: string[][] = [];
  for (const line of text.split('\n')) {
    if (BULLET.test(line)) items.push([line.replace(BULLET, '')]);
    else if (items.length && line.trim()) items.at(-1)!.push(line.trim());
  }
  return items.map((l, n) => ({ id: `item-${n + 1}`, text: l.join(' ') }));
}

// Bullets and headings end a sentence as a full stop does.
function sentences(text: string, splitOn?: string): Chunk[] {
  const lines = text.split('\n').map((l) => l.replace(BULLET, '').replace(/^#+\s.*$/, '').trim()).filter(Boolean);
  const out = lines.flatMap((l) => l.split(/(?<=[.!?])\s+(?=[A-Z`"(])/)).flatMap((s) => (splitOn ? s.split(new RegExp(splitOn, 'i')) : [s]));
  return out.map((s) => s.replace(/[?]+$/, '').trim()).filter(Boolean).map((s, n) => ({ id: `s${n + 1}`, text: s }));
}

function jsonItems(inputs: Inputs, o: SplitDecl): Chunk[] {
  const root = at(JSON.parse(from(inputs, o.from)), o.path);
  const entries: Array<[string, unknown]> = Array.isArray(root) ? root.map((v, n) => [String(isObj(v) && (typeof v.id === 'string' || typeof v.path === 'string') ? v.id ?? v.path : n), v]) : isObj(root) ? Object.entries(root) : root === undefined ? [] : [['0', root]];
  const shown = (v: unknown) => (o.pick && isObj(v) ? Object.fromEntries(o.pick.map((p) => [p, at(v, p)])) : v);
  return entries.map(([k, v]) => ({ id: k, key: k, text: typeof v === 'string' ? v : JSON.stringify(shown(v)), meta: isObj(v) ? v : { value: v } }));
}

function diffFiles(inputs: Inputs, o: SplitDecl): Chunk[] {
  const files = from(inputs, o.from).split(/^(?=diff --git )/m).filter((p) => p.startsWith('diff --git'));
  const chunks = files.map((p) => {
    const path = /^diff --git a\/(\S+)/.exec(p)![1];
    return { id: path, key: path, text: p, meta: { added: /^new file mode/m.test(p) } };
  });
  return o.filter === 'added' ? chunks.filter((c) => c.meta.added) : chunks;
}

const diffHunks = (inputs: Inputs, o: SplitDecl): Chunk[] =>
  diffFiles(inputs, o).flatMap((f) => f.text.split(/^(?=@@ )/m).slice(1).map((h, n) => ({ id: `${f.id}#${n + 1}`, key: f.key, text: `${f.key} ${h}` })));

// Map-reduce digests: facts by class, and the passage's other words as its terms.
type Fact = { cls: string; value: string };
type Digest = { facts: Fact[]; terms: Set<string> };
const STOP = new Set(['that', 'this', 'with', 'from', 'have', 'which', 'what', 'when', 'will', 'does', 'into', 'also', 'than', 'then', 'there', 'their', 'each', 'only', 'more', 'most', 'over', 'such', 'both', 'been', 'were', 'they', 'them', 'where', 'while', 'about', 'after', 'before']);
export const words = (s: string) => new Set((s.toLowerCase().match(/[a-z][a-z0-9]{3,}/g) ?? []).filter((w) => !STOP.has(w)));
const MONEY = /\$\s?\d[\d,.]*(?:\s?(?:million|billion|thousand|k|m))?/gi;
const TIME = /\b\d{1,2}(?::\d\d)?\s?(?:am|pm)\b/gi;
const DATE = /\b(?:\d{1,2}\s)?(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?(?:\s\d{1,2})?,?\s\d{4}\b|\b(?:19|20)\d\d\b/gi;
const NUM = /\b\d[\d,]*(?:\.\d+)?\b(?:\s?(?:%|percent|km|hectares|days|months|years|metres|meters|m\b))?/gi;
const norm = (s: string) => s.toLowerCase().replace(/[\s,]/g, '');

const extractors: Record<string, (text: string) => Digest> = {
  /** Money, times, dates and plain quantities, each tagged with a class; the rest of the passage is its terms. */
  numbers: (text) => {
    const facts: Fact[] = [];
    let rest = text;
    for (const [cls, re] of [['money', MONEY], ['time', TIME], ['date', DATE]] as const) {
      for (const m of rest.matchAll(re)) facts.push({ cls, value: norm(m[0]) });
      rest = rest.replace(re, ' ');
    }
    for (const m of rest.matchAll(NUM)) facts.push({ cls: 'number', value: norm(m[0]) });
    return { facts, terms: words(rest.replace(NUM, ' ')) };
  },
};

const valuesOf = (d: Digest, cls: string) => new Set(d.facts.filter((f) => f.cls === cls).map((f) => f.value));
const within = (p: Set<string>, q: Set<string>) => [...p].every((v) => q.has(v));

const comparators: Record<string, (a: Digest, b: Digest, minShared: number) => number> = {
  /** The shared-term count when, in a fact class both have, neither value set contains the other; else 0. */
  sameTermsOtherValue: (a, b, minShared) => {
    const shared = [...a.terms].filter((t) => b.terms.has(t)).length;
    if (shared < minShared) return 0;
    const clash = [...new Set(a.facts.map((f) => f.cls))].some((cls) => {
      const [x, y] = [valuesOf(a, cls), valuesOf(b, cls)];
      return x.size > 0 && y.size > 0 && !within(x, y) && !within(y, x);
    });
    return clash ? shared : 0;
  },
};

function strategy<T>(table: Record<string, T>, kind: string, name: string | undefined): T {
  const s = name === undefined ? undefined : table[name];
  if (!s) throw new Error(`no ${kind} "${name}" in the judge's library`);
  return s;
}

/** Pairs of passages whose digests may conflict, best `top` first; the check's fold is the reduce over them. */
function digestPairs(inputs: Inputs, o: SplitDecl): Chunk[] {
  if (!o.of) throw new Error('digestPairs needs `of`, the splitter whose passages it compares');
  const pieces = runSplitter(inputs, o.of);
  const extract = strategy(extractors, 'digest extractor', o.extract);
  const compare = strategy(comparators, 'comparator', o.compare);
  const digests = pieces.map((p) => extract(p.text));
  const pairs = pieces.flatMap((a, x) => pieces.slice(x + 1).map((b, y) => ({ a, b, s: compare(digests[x], digests[x + 1 + y], o.minShared ?? 2) })));
  const best = pairs.filter((p) => p.s > 0).sort((p, q) => q.s - p.s).slice(0, o.top ?? pairs.length);
  return best.map(({ a, b }) => ({ id: `${a.id}|${b.id}`, text: JSON.stringify({ first: a.text, second: b.text }) }));
}

type Splitter = (inputs: Inputs, o: SplitDecl) => Chunk[];

const library: Record<string, Splitter> = {
  /** One item: the input (or inputs) as it is. */
  whole: (i, o) => [{ id: [o.from].flat().join('+'), text: from(i, o.from) }],
  jsonItems,
  /** `## ` sections, by heading. */
  markdownSections: (i, o) => from(i, o.from).split(/^## /m).slice(1).map((p) => ({ id: head(p), key: head(p), text: body(p).trim() })),
  listItems: (i, o) => listItems(scoped(i, o)),
  sentences: (i, o) => sentences(scoped(i, o), o.splitOn),
  diffFiles,
  diffHunks,
  digestPairs,
};

const registered = new Map<string, Splitter>();
const registeredSelectors = new Map<string, (inputs: Inputs, chunk: Chunk, options: SelectorDecl) => string | undefined>();

function register<T>(table: Map<string, T>, kind: string, name: string, run: T, builtIn: boolean) {
  if (builtIn) throw new Error(`"${name}" is the library's own ${kind}`);
  // Registrations are process-wide: a second, different strategy under one name would judge with whichever loaded last.
  // A replay loads one extension twice (its arm at another ref), so the same source registering again is the same strategy.
  const prior = table.get(name);
  if (prior && prior !== run && String(prior) !== String(run)) throw new Error(`a different ${kind} is already registered as "${name}"`);
  table.set(name, run);
}

/** Registers a named splitter for an input the library's splitters do not shape. */
export function registerSplitter(name: string, run: Splitter): void {
  register(registered, 'splitter', name, run, name in library || name === 'llm');
}

/** Registers a named context selector the library does not offer; returning undefined drops the item from the scope. */
export function registerSelector(name: string, run: (inputs: Inputs, chunk: Chunk, options: SelectorDecl) => string | undefined): void {
  register(registeredSelectors, 'selector', name, run, name in selectors);
}

const letters = /[\p{L}\p{N}]/u;
const wordCount = (s: string) => s.split(/\s+/).filter(Boolean).length;

/** The item filters every split applies after it runs, in declaration order of the options. */
export function keep(chunks: Chunk[], o: SplitDecl): Chunk[] {
  const names = o.names?.map((n) => n.toLowerCase());
  const match = o.match ? new RegExp(o.match, 'i') : undefined;
  const strip = o.strip ? new RegExp(o.strip, 'g') : undefined;
  const plain = (o.where ?? []).filter((w) => !('as' in w));
  const out = chunks
    .filter((c) => !names || names.includes(c.id.toLowerCase()))
    .filter((c) => plain.every((w) => holds(w, c.meta ?? {})))
    .map((c) => (strip ? { ...c, text: c.text.replace(strip, '').replace(/[ \t]+/g, ' ').trim() } : c))
    .filter((c) => letters.test(c.text) && wordCount(c.text) >= (o.minWords ?? 1) && (!match || match.test(c.text)));
  return o.max === undefined ? out : out.slice(0, o.max);
}

/** A deterministic split with its filters: a library splitter or a registered one. */
export function runSplitter(inputs: Inputs, o: SplitDecl): Chunk[] {
  const run = library[o.name] ?? registered.get(o.name);
  if (!run) throw new Error(o.name === 'llm' ? 'the llm splitter cannot run here (inside map-reduce or a related selector)' : `no splitter "${o.name}" in the judge's library or registered`);
  return keep(run(inputs, o), o);
}

const same = (a: unknown, b: unknown) => a === b || JSON.stringify(a) === JSON.stringify(b);

/** Whether an item's fields pass a filter; the cross-input form compares against the record joined under `as`. */
export function holds(w: Where, item: unknown, joined?: unknown): boolean {
  const v = at(item, w.path);
  if (w.op === 'truthy' || w.op === 'falsy') return (Array.isArray(v) ? v.length > 0 : !!v) === (w.op === 'truthy');
  const other = 'as' in w ? at(joined, w.field) : (w as { value: unknown }).value;
  return same(v, other) === (w.op === 'eq');
}

// Matchers score how well another chunk of the related input matches the item; 0 is no match.
const tail = (s: string) => s.split('/').pop()!;
const dir = (s: string) => s.slice(0, s.lastIndexOf('/') + 1);
// The name suffix siblings share: "a.test.ts" -> ".test.ts", "store_pg.go" -> "pg.go".
const suffix = (s: string) => {
  const b = tail(s);
  return b.split('.').length > 2 ? b.slice(b.indexOf('.')) : `${b.slice(0, b.lastIndexOf('.')).split(/[-_]/).pop()}${b.slice(b.lastIndexOf('.'))}`;
};
const names = (text: string, key?: string) => !!key && (text.includes(key) || text.includes(tail(key)));
const matchers: Record<string, (piece: Chunk, other: Chunk) => number> = {
  /** The other chunk's key (a file path) is named in the piece. */
  names: (p, r) => (names(p.text, r.key) ? 1 : 0),
  /** The piece's key is named in the other chunk. */
  cites: (p, r) => (names(r.text, p.key) ? 1 : 0),
  /** Words shared with the piece. */
  overlap: (p, r) => {
    const w = words(p.text);
    return [...words(`${r.key ?? ''} ${r.text}`)].filter((x) => w.has(x)).length;
  },
  /** Same directory and name suffix as the piece's file; newer first by `meta.ts`. */
  sibling: (p, r) => (p.key && r.key && p.key !== r.key && dir(p.key) === dir(r.key) && suffix(p.key) === suffix(r.key) ? 1 + Number(r.meta?.ts ?? 0) / 1e12 : 0),
};

/** A selector's value for an item, and how many matches it found (for `related` and `join`). */
export type Selected = { value: string | undefined; matches?: number };
type Selector = (inputs: Inputs, chunk: Chunk, o: SelectorDecl, split: (o: SplitDecl) => Chunk[]) => Selected;
type Of<N extends string> = Extract<SelectorDecl, { name: N }>;

// A declared cut is marked in the text, so the model knows it was shown part of the context.
const cut = (s: string, max?: number) => (max !== undefined && s.length > max ? `${s.slice(0, max)}\n… (cut at ${max} characters)` : s);

function jsonPart(inputs: Inputs, chunk: Chunk, o: Of<'json'>): string {
  const pointer = typeof o.pointer === 'object' ? String(at(chunk.meta, o.pointer.field) ?? '') : o.pointer ?? '';
  const step = (x: unknown, k: string) => (isObj(x) || Array.isArray(x) ? (x as Obj)[k.replaceAll('~1', '/').replaceAll('~0', '~')] : undefined);
  const part = pointer.split('/').slice(1).reduce<unknown>(step, JSON.parse(input(inputs, o.from)));
  const shown = o.pick && isObj(part) ? Object.fromEntries(o.pick.map((p) => [p, at(part, p)])) : part;
  return JSON.stringify(shown ?? null);
}

function related(inputs: Inputs, chunk: Chunk, o: Of<'related'>, split: (o: SplitDecl) => Chunk[]): Selected {
  const others = split(o.chunker);
  const ranked = (m: string) => {
    const score = strategy(matchers, 'matcher', m);
    return others.map((r) => ({ r, s: score(chunk, r) })).filter((x) => x.s >= (o.min ?? 1)).sort((a, b) => b.s - a.s).slice(0, o.top ?? 3);
  };
  // Matchers are tried in order; the first that finds anything wins.
  const hits = [o.match].flat().map(ranked).find((h) => h.length) ?? [];
  if (!hits.length) return { value: o.require ? undefined : '(none)', matches: 0 };
  const shown = hits.map(({ r }) => (r.key && !r.text.startsWith(r.key) ? `${r.key}: ${r.text}` : r.text)).join('\n---\n');
  return { value: cut(shown, o.maxChars), matches: hits.length };
}

/** The record a `join` looks up for an item: the item's `by` field, or its key, in the JSON object of the second input. */
export function joined(inputs: Inputs, chunk: Chunk, o: Of<'join'>): unknown {
  const key = o.by ? at(chunk.meta, o.by) : chunk.key;
  const table = JSON.parse(input(inputs, o.from));
  return key === undefined || !isObj(table) ? undefined : table[String(key)];
}

const selectors: Record<string, Selector> = {
  whole: (i, _c, o) => ({ value: cut(input(i, (o as Of<'whole'>).from), (o as Of<'whole'>).maxChars) }),
  sections: (i, _c, o) => ({ value: cut(sectionText(input(i, (o as Of<'sections'>).from), (o as Of<'sections'>).names), (o as Of<'sections'>).maxChars) }),
  items: (i, _c, o) => {
    const it = o as Of<'items'>;
    return { value: cut(keep(listItems(scoped(i, it)), { name: 'items', from: it.from, strip: it.strip }).map((c) => `- ${c.text}`).join('\n'), it.maxChars) };
  },
  json: (i, c, o) => ({ value: jsonPart(i, c, o as Of<'json'>) }),
  related: (i, c, o, split) => related(i, c, o as Of<'related'>, split),
  join: (i, c, o) => {
    const record = joined(i, c, o as Of<'join'>);
    return { value: record === undefined ? '(none)' : JSON.stringify(record), matches: record === undefined ? 0 : 1 };
  },
};

/** Whether a selector shows every item of its scope the same context, so it can go on the units' `shared`. */
export const itemIndependent = (o: SelectorDecl) => ['whole', 'sections', 'items'].includes(o.name) || (o.name === 'json' && typeof (o as Of<'json'>).pointer !== 'object');

export function select(inputs: Inputs, chunk: Chunk, o: SelectorDecl, split: (o: SplitDecl) => Chunk[]): Selected {
  const own = selectors[o.name];
  if (own) return own(inputs, chunk, o, split);
  const run = registeredSelectors.get(o.name);
  if (!run) throw new Error(`no context selector "${o.name}" in the judge's library or registered`);
  return { value: run(inputs, chunk, o) };
}
