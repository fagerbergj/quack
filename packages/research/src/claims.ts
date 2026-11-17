// What code can read of a report's claims without a model: the figures a sentence states, and whether two sentences may state one thing two ways.

/** A figure as shown, with the words that follow it (its unit) and the words around it (what it measures). */
export type Figure = { value: string; unit: Set<string>; near: Set<string> };

const STOP = new Set('the and for per of to a an in at on or vs is are by it its as be with that this from have has which what when will does into also than then there their each only more most over such both been were they them where while about after before your these those would could should other under being every using used make made need very much many same just like within without here not no can may all any one'.split(' '));
const UNITS = /(\d)(am|pm|mm|cm|in|ft|lb|lbs|kg|u|ru|k|m|gb|tb|mb|w|v|hz|%|")(?![\p{L}\d])/giu;
const wordsOf = (ts: string[]) => new Set(ts.flatMap((t) => t.toLowerCase().split(/[-/]/)).map((w) => w.replace(/[^\p{L}%$"]/gu, '')).filter((w) => w && !STOP.has(w)));
// Marks and emphasis are not words of the claim; a unit written onto its number ("24U", "10am") is a word of its own.
const tokens = (text: string) => text.replace(/\[s\d+\]/g, ' ').replace(/[*_`]/g, '').replace(UNITS, '$1 $2').split(/[\s;:()[\]]+|,(?!\d)/).filter(Boolean);

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
    return m ? [{ value: m[2].replace(/,/g, ''), unit, near: wordsOf([...ts.slice(Math.max(0, i - 3), i), ...ts.slice(i + 1, i + 4), ...date]) }] : [];
  });
}

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
const RACK_UNIT = '44.45';
const NUM = String.raw`[~≈]?\$?\d[\d,]*(?:\.\d+)?`;
const UNIT = String.raw`(?:\s?(?:mm|cm|m|in|ft|lbs|lb|kg|g|RU|U|")(?![\p{L}\d]))?`;
// An en dash is a range (2–6 U), and a hyphen or an x is an operator only between spaces.
const OP = String.raw`(?:\s*[+×*/÷−]\s*|\s+[x-]\s+)`;
const TERMS = `${NUM}${UNIT}(?:${OP}${NUM}${UNIT})*`;
// A chain starts at no operator, so "566 mm (case height) ÷ 44.45 = 12.7U" is no "44.45 = 12.7U"; a value shown as a range
// ("≈ 31–32U") or as the start of another expression ("= 1,189 × 600 mm") is not one value, so the chain stops before it.
const CHAIN = new RegExp(String.raw`(?<![\p{L}\d.,])(?<!(?:[+×*/÷−=≈]|\s[x-])\s*)${TERMS}(?:\s*[=≈]\s*${NUM}${UNIT}(?![\d,.]*(?:\s*[–—-]\s*\d|${OP})))+`, 'gu');
const PAREN = new RegExp(String.raw`(?<![\p{L}\d.,])(${NUM}${UNIT})\s*\(\s*(${TERMS})\s*\)`, 'gu');
const TERM = new RegExp(String.raw`(${NUM})(?:\s?(mm|cm|m|in|ft|lbs|lb|kg|g|RU|U|")(?![\p{L}\d]))?|([+×*/÷−x-])`, 'gu');

type Term = { value: string; unit?: string };
const parse = (s: string) => {
  const ms = [...s.matchAll(TERM)];
  return { terms: ms.filter((m) => m[1]).map((m): Term => ({ value: m[1].replace(/[~≈$,]/g, ''), unit: m[2]?.toLowerCase() })), ops: ms.filter((m) => m[3]).map((m) => m[3]) };
};

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
  const [first, ...rest] = text.split(/\s*[=≈]\s*/).map(parse);
  const shown = rest.map((s) => s.terms[0]);
  const values = first.terms.length > 1 ? shown : [first.terms[0], ...shown];
  return [...(first.terms.length > 1 ? [expression(text, first, shown[0])] : []), ...values.slice(1).flatMap((v, k) => conversion(text, values[k], v))];
}

/** "c (a × b)" checks the expression against c; "x mm (y in)" checks the conversion. */
function paren(text: string, outer: string, inner: string): Derivation[] {
  const [o, i] = [parse(outer).terms[0], parse(inner)];
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
