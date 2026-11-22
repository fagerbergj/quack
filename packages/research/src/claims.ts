// What code can read of a report's claims without a model: the figures a sentence states, and whether two sentences may state one thing two ways.

/** A figure as shown, with the words that follow it (its unit), the words around it (what it measures) and the unit written right after it. */
export type Figure = { value: string; unit: Set<string>; near: Set<string>; unitToken: string };

const STOP = new Set('the and for per of to a an in at on or vs is are by it its as be with that this from have has which what when will does into also than then there their each only more most over such both been were they them where while about after before your these those would could should other under being every using used make made need very much many same just like within without here not no can may all any one'.split(' '));
const UNITS = /(\d)(am|pm|mm|cm|in|ft|lb|lbs|kg|u|ru|k|m|gb|tb|mb|w|v|hz|%|")(?![\p{L}\d])/giu;
const wordsOf = (ts: string[]) => new Set(ts.flatMap((t) => t.toLowerCase().split(/[-/]/)).map((w) => w.replace(/[^\p{L}%$"]/gu, '')).filter((w) => w && !STOP.has(w)));
// Marks and emphasis are not words of the claim; a unit written onto its number ("24U", "10am", "10-ft") is a word of its own.
const tokens = (text: string) => text.replace(/\[s\d+\]/g, ' ').replace(/[*_`]/g, '').replace(/(\d)-(?=\p{L})/gu, '$1 ').replace(UNITS, '$1 $2').split(/[\s;:()[\]]+|,(?!\d)/).filter(Boolean);

const MONTHS = ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'];
const MONTH = String.raw`(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\.?`;
const iso = (y: string, m: string, d: string) => `${y}-${String(MONTHS.indexOf(m.toLowerCase()) + 1).padStart(2, '0')}-${d.padStart(2, '0')}`;
// One date written two ways is one figure: "October 20, 2027" and "20 Oct 2027" read as 2027-10-20, as an ISO date does.
const isoDates = (text: string) => text
  .replace(new RegExp(String.raw`\b${MONTH}\s+(\d{1,2}),?\s+(\d{4})\b`, 'g'), (_, m: string, d: string, y: string) => iso(y, m, d))
  .replace(new RegExp(String.raw`\b(\d{1,2})\s+${MONTH}\s+(\d{4})\b`, 'g'), (_, d: string, m: string, y: string) => iso(y, m, d));
// "(1) … (2) …" numbers a list and "Option 2 —" labels an item: neither states a quantity. "(2) Keys, (4) Casters" counts, so only a run 1, 2, 3… is a list.
function unnumbered(text: string): string {
  const ns = [...text.matchAll(/\((\d{1,2})\)/g)].map((m) => Number(m[1]));
  const listed = ns.length > 1 && ns.every((n, i) => n === i + 1) ? text.replace(/\(\d{1,2}\)/g, ' ') : text;
  return listed.replace(/(?<=\b\p{Lu}\p{Ll}+ )\d{1,2}(?=\s*[—–:])/gu, ' ');
}

/** Every number standing on its own (not inside a model code such as AR3104 or IP54), as shown without its thousands commas; a date as ISO. */
export function figures(text: string): Figure[] {
  const ts = tokens(unnumbered(isoDates(text.replace(/[*_`]/g, ''))));
  return ts.flatMap((t, i) => {
    const m = /^[~≈≥≤<>±+]*(\$?)(\d{4}-\d\d-\d\d|\d[\d,]*(?:\.\d+)?)\.?$/.exec(t);
    // A currency sign is the figure's unit as much as a word after it is; a date's unit is "date", which its month and year were.
    const date = m?.[2].includes('-') ? ['date'] : [];
    const unit = wordsOf([...ts.slice(i + 1, i + 3), m?.[1] ?? '', ...date]);
    // A sentence's period or a closing quote stays on the token after a figure ("in."); it is no part of the unit.
    const unitToken = m?.[1] ? '$' : (/^(\p{L}+|")/u.exec((ts[i + 1] ?? '').toLowerCase())?.[1] ?? '');
    return m ? [{ value: m[2].replace(/,/g, ''), unit, near: wordsOf([...ts.slice(Math.max(0, i - 3), i), ...ts.slice(i + 1, i + 4), ...date]), unitToken }] : [];
  });
}

const NUMBER_WORDS = ['two', 'three', 'four', 'five', 'six', 'seven', 'eight', 'nine', 'ten', 'eleven', 'twelve'];
/** The counts a text writes as words ("Eight fans"), as the figures they are; "one" is left out, as it is mostly not a count. */
export const numberWords = (text: string) => [...text.toLowerCase().matchAll(/\b(two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)\b/g)].map((m) => String(NUMBER_WORDS.indexOf(m[1]) + 2));

const decimals = (v: string) => v.split('.')[1]?.length ?? 0;
// A date's year is the year another passage gives alone: "in 2026" and "5 January 2026".
const yearOf = (v: string) => (/^\d{4}-\d\d-\d\d$/.test(v) ? v.slice(0, 4) : v);
/** Whether two shown values are one value: the more precise rounds to the other, so 1,066.8 is the ~1,067 of another passage. */
export const sameValue = (a: string, b: string) => a === b || yearOf(a) === b || a === yearOf(b) || Math.abs(Number(a) - Number(b)) < 10 ** -Math.min(decimals(a), decimals(b)) / 2 + 1e-9;

const meets = (a: Set<string>, b: Set<string>) => [...a].some((w) => b.has(w));
const NEAR = 2;

/** Two figures of one quantity with different values: a unit word in common and two words nearby in common, and neither passage also
 *  giving the other's value, since a passage that gives both reports the difference rather than taking a side. */
export function figuresClash(a: Figure[], b: Figure[]): boolean {
  const has = (fs: Figure[], f: Figure) => fs.some((g) => sameValue(g.value, f.value));
  return a.some((x) => b.some((y) => !sameValue(x.value, y.value) && meets(x.unit, y.unit) && !has(a, y) && !has(b, x) && [...x.near].filter((w) => y.near.has(w)).length >= NEAR));
}

const NAMED = /(\p{L}+)[\s,]+(\p{Lu}[\p{L}&'’-]+(?:\s+\p{Lu}[\p{L}&'’-]+)*)/gu;
const namesOf = (text: string) => [...tokens(text).join(' ').matchAll(NAMED)].filter((m) => /^\p{Ll}/u.test(m[1]) && !STOP.has(m[1])).map((m) => ({ after: m[1], name: m[2].toLowerCase() }));

/** Two names for one role: each passage names someone right after the same word ("the contractor, Harbour Works"), and neither names the other's. */
export function namesClash(a: string, b: string): boolean {
  const [x, y] = [namesOf(a), namesOf(b)];
  return x.some((p) => y.some((q) => p.after === q.after && p.name !== q.name && !y.some((r) => r.name === p.name) && !x.some((r) => r.name === q.name)));
}

/** One figure the report derives and code can recompute: its inputs as shown, the value it shows, and the value its inputs give. */
export type Derivation = { text: string; inputs: string[]; shown: string; computed: number };

// Each unit's measure in a base unit of its kind; 1U is the EIA-310 rack unit, so a U figure also takes 44.45 as an input.
const SCALE: Record<string, [string, number]> = { mm: ['length', 1], cm: ['length', 10], m: ['length', 1000], in: ['length', 25.4], '"': ['length', 25.4], ft: ['length', 304.8], u: ['length', 44.45], ru: ['length', 44.45], kg: ['mass', 1], g: ['mass', 0.001], lb: ['mass', 0.45359237], lbs: ['mass', 0.45359237] };
export const RACK_UNIT = '44.45';
const NUM = String.raw`[~≈]?\$?\d[\d,]*(?:\.\d+)?`;
const UNIT = String.raw`(?:\s?(?:mm|cm|m|in|ft|lbs|lb|kg|g|RU|U|")(?![\p{L}\d]))?`;
// An en dash is a range (2–6 U), and a hyphen or an x is an operator only between spaces.
const OP = String.raw`(?:\s*[+×*/÷−]\s*|\s+[x-]\s+)`;
// An operand may carry a label between the operator and its number ("566 mm + Define 5 ~460 mm + mATX ~200 mm"): up to four words, a
// word followed by a small model number when another number follows.
const LABEL = String.raw`(?:\p{L}[\p{L}\d.'’-]*\s+(?:\d{1,2}\s+(?=[~≈$]?\d))?){1,4}`;
const TERMS = `${NUM}${UNIT}(?:${OP}(?:${LABEL})?${NUM}${UNIT})*`;
// A chain starts at no operator, so "566 mm (case height) ÷ 44.45 = 12.7U" is no "44.45 = 12.7U"; a value shown as a range
// ("≈ 31–32U") or as the start of another expression ("= 1,189 × 600 mm") is not one value, so the chain stops before it.
const CHAIN = new RegExp(String.raw`(?<![\p{L}\d.,])(?<!(?:[+×*/÷−=≈]|\s[x-])\s*)${TERMS}(?:\s*[=≈]\s*${NUM}${UNIT}(?![\d,.]*(?:\s*[–—-]\s*\d|${OP})))+`, 'gu');
const PAREN = new RegExp(String.raw`(?<![\p{L}\d.,])(${NUM}${UNIT})\s*\(\s*(${TERMS})\s*\)`, 'gu');
const TERM = new RegExp(String.raw`(${NUM})(?:\s?(mm|cm|m|in|ft|lbs|lb|kg|g|RU|U|")(?![\p{L}\d]))?`, 'gu');
const OPS = new RegExp(OP, 'gu');

type Term = { value: string; unit?: string };
/** An expression's operators and operands; an operand is the last figure of its segment, so a label's model number ("Define 5") is not one.
 *  undefined when a segment holds no figure: a shape code cannot read is no derivation. */
function parse(s: string): { terms: Term[]; ops: string[] } | undefined {
  const found = s.split(OPS).map((seg) => [...seg.matchAll(TERM)].at(-1));
  if (found.some((m) => !m)) return undefined;
  return { terms: found.map((m) => ({ value: m![1].replace(/[~≈$,]/g, ''), unit: m![2]?.toLowerCase() })), ops: [...s.matchAll(OPS)].map((m) => m[0].trim()) };
}
// "=" or "≈" separates two sides only after a figure ends; "≈" before a figure ("566 + ≈460") marks it as approximate.
const SIDES = /(?<=[\d\p{L}"%])\s*[=≈]\s*/u;

/** Products and quotients first, then sums and differences, left to right. */
function evaluate(values: number[], ops: string[]): number {
  const sums = [values[0]];
  const signs: string[] = [];
  ops.forEach((op, i) => {
    if ('×*x/÷'.includes(op)) sums.push(op === '/' || op === '÷' ? sums.pop()! / values[i + 1] : sums.pop()! * values[i + 1]);
    else (signs.push(op), sums.push(values[i + 1]));
  });
  return sums.slice(1).reduce((acc, v, i) => (signs[i] === '+' ? acc + v : acc - v), sums[0]);
}

const rack = (t: Term) => t.unit === 'u' || t.unit === 'ru';
// A whole number of U beside a length names a cabinet's size class ("a 12U (634 mm) cabinet"), not a conversion; 11.27 U is one.
const sizeClass = (a: Term, b: Term) => [a, b].some((t) => rack(t) && !t.value.includes('.'));

/** The value of `t` in the unit of `to`, with the inputs that adds; undefined when the two are not one quantity code can convert. */
function convert(t: Term, to: Term): { value: number; inputs: string[] } | undefined {
  const [a, b] = [SCALE[t.unit ?? ''], SCALE[to.unit ?? '']];
  if (!a || !b || a[0] !== b[0] || sizeClass(t, to)) return undefined;
  return { value: (Number(t.value) * a[1]) / b[1], inputs: rack(t) || rack(to) ? [RACK_UNIT] : [] };
}

/** Two figures shown as one quantity in two units, in whichever direction recomputes, since the text does not say which was the input. */
function conversion(text: string, a: Term, b: Term): Derivation[] {
  const [there, back] = [convert(a, b), convert(b, a)];
  if (!a.unit || !b.unit || a.unit === b.unit || !there || !back) return [];
  const ab = { text, inputs: [a.value, ...there.inputs], shown: b.value, computed: there.value };
  const ba = { text, inputs: [b.value, ...back.inputs], shown: a.value, computed: back.value };
  return [recomputes(ba) && !recomputes(ab) ? ba : ab];
}

const expression = (text: string, e: { terms: Term[]; ops: string[] }, shown: Term): Derivation => ({ text, inputs: e.terms.map((t) => t.value), shown: shown.value, computed: evaluate(e.terms.map((t) => Number(t.value)), e.ops) });

/** "a + b = c (= d)": the expression against the first value shown, then each value shown against the one before it in another unit.
 *  An expression's units travel through its arithmetic, so its result is compared as shown. */
function chain(text: string): Derivation[] {
  const sides = text.split(SIDES).map(parse);
  if (sides.length < 2 || sides.some((p) => !p)) return [];
  const [first, ...rest] = sides as Array<{ terms: Term[]; ops: string[] }>;
  const shown = rest.map((s) => s.terms[0]);
  const values = first.terms.length > 1 ? shown : [first.terms[0], ...shown];
  return [...(first.terms.length > 1 ? [expression(text, first, shown[0])] : []), ...values.slice(1).flatMap((v, k) => conversion(text, values[k], v))];
}

/** "c (a × b)" checks the expression against c; "x mm (y in)" checks the conversion. */
function paren(text: string, outer: string, inner: string): Derivation[] {
  const [p, i] = [parse(outer), parse(inner)];
  if (!p || !i) return [];
  const o = p.terms[0];
  if (i.terms.length === 1) return conversion(text, o, i.terms[0]);
  // "38.5 in (97.79 × 59.95 cm)" gives dimensions in another unit; only an expression in c's own unit, or in none, is c's.
  const unit = i.terms.findLast((t) => t.unit)?.unit;
  return unit && unit !== o.unit ? [] : [expression(text, i, o)];
}

/** Every equation, parenthesised expression and unit conversion the text shows, marks and emphasis aside. */
export function derivations(text: string): Derivation[] {
  const plain = text.replace(/\[s\d+\]/g, '').replace(/[*_`]/g, '');
  return [...[...plain.matchAll(CHAIN)].flatMap((m) => chain(m[0])), ...[...plain.matchAll(PAREN)].flatMap((m) => paren(m[0], m[1], m[2]))];
}

/** Whether the value shown is the value computed at the precision shown: within one in its last shown digit. */
export const recomputes = (d: Derivation) => Math.abs(d.computed - Number(d.shown)) <= 10 ** -decimals(d.shown) + 1e-9;

/** Every number a text shows outside a model code (AR3104, IP54) or a link: a range's ends, a quoted "$1,401.99" or a "$159/month" too,
 *  which figures() reads as no figure. */
export const bareNumbers = (text: string) => [...isoDates(text.replace(/\]\([^)]*\)|https?:\/\/\S+/g, ' ')).matchAll(/(?<![\p{L}\d.,-])(?:\d{4}-\d\d-\d\d|\d[\d,]*(?:\.\d+)?)/gu)].map((m) => m[0].replace(/,/g, ''));

/** Whether `v` restates `known`: one value, shown no more precisely, so 1,067 restates 1,066.8 but 44.45 does not restate a 44. */
export const restates = (v: string, known: string) => sameValue(known, v) && decimals(known) >= decimals(v);

const ALIAS: Record<string, string> = { lbs: 'lb', ru: 'u', '"': 'in' };
/** The unit a figure is written in, when code can tell: a currency sign, or a length or mass unit right after it ("lbs" is "lb"). */
export const measureOf = (f: Figure) => (f.unitToken === '$' ? '$' : SCALE[f.unitToken] ? (ALIAS[f.unitToken] ?? f.unitToken) : undefined);

/** A traced figure: its value and, when code can tell, its unit, so a reached 140 mm does not trace a $140. */
export type Known = { value: string; unit?: string };
/** Whether `v`, written in `unit`, restates `k`; a figure whose unit code cannot tell matches any unit. */
export const restatesKnown = (v: string, unit: string | undefined, k: Known) => restates(v, k.value) && !(unit && k.unit && unit !== k.unit);

// A figure's unit: its currency sign or the convertible unit right after it, else its first word after it ("CFM"); none, and it takes
// the next figure's, as in "566 + 460 + 300 = 1,326 mm" or "1070 vs 978 mm".
function unitsOf(fs: Figure[]): Array<string | undefined> {
  const own = fs.map((f) => measureOf(f) ?? [...f.unit][0]);
  return own.map((u, i) => u ?? own.slice(i + 1).find(Boolean));
}

/** A value in unit `from` as one in unit `to`, if code can tell they measure one kind of thing; a U conversion needs the rack unit traced. */
function convertTo(value: number, from: string | undefined, to: string, rackUnit: boolean): number | undefined {
  if (from === to) return value;
  const [a, b] = [SCALE[from ?? ''], SCALE[to]];
  const viaRack = [from, to].some((u) => u === 'u' || u === 'ru');
  return a && b && a[0] === b[0] && (rackUnit || !viaRack) ? (value * a[1]) / b[1] : undefined;
}

/** Figures of `text` that are arithmetic in words over traced figures of `text` and `context`: one of them in another unit ("24U (1,066.8 mm)"),
 *  or the sum or difference of two ("~140 mm margin" beside 743 and 604 mm), at the precision shown. Wider scopes clear untraced inputs by chance. */
export function reached(text: string, context: string, traced: (v: string, unit?: string) => boolean, rackUnit: boolean): Known[] {
  const [own, before] = [figures(text), figures(context)];
  const all = [...before, ...own];
  const units = unitsOf(all);
  const out: Known[] = [];
  const known = (i: number) => traced(all[i].value, measureOf(all[i])) || out.some((o) => restatesKnown(all[i].value, measureOf(all[i]), o));
  own.forEach((f, k) => {
    const [i, to] = [before.length + k, units[before.length + k]];
    if (!to || known(i)) return;
    // Only an operand already in `to` or converted into it survives, so its unit has a SCALE entry unless it is `to` itself.
    const vs = all.flatMap((g, j) => (j !== i && known(j) ? [{ v: convertTo(Number(g.value), units[j], to, rackUnit), unit: units[j] }] : [])).filter((o): o is { v: number; unit: string | undefined } => o.v !== undefined);
    const hit = (computed: number) => recomputes({ text, inputs: [], shown: f.value, computed });
    // A sum converts into a finer unit only: summed from millimetres, a whole U or inch is within ±1 of something by chance.
    const parts = vs.filter((o) => o.unit === to || SCALE[o.unit ?? ''][1] >= SCALE[to][1]).map((o) => o.v);
    const sums = parts.some((a, n) => parts.slice(n + 1).some((b) => hit(a + b) || hit(Math.abs(a - b))));
    if (vs.some((o) => hit(o.v)) || sums) out.push({ value: f.value, unit: measureOf(f) });
  });
  return out;
}
