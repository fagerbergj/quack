// What code can read of a report's claims without a model: the figures a sentence states, and whether two sentences may state one thing two ways.

/** A figure as shown, with the words that follow it (its unit) and the words around it (what it measures). */
export type Figure = { value: string; unit: Set<string>; near: Set<string> };

const STOP = new Set('the and for per of to a an in at on or vs is are by it its as be with that this from have has which what when will does into also than then there their each only more most over such both been were they them where while about after before your these those would could should other under being every using used make made need very much many same just like within without here not no can may all any one'.split(' '));
const UNITS = /(\d)(am|pm|mm|cm|in|ft|lb|lbs|kg|u|ru|k|m|gb|tb|mb|w|v|hz|%|")(?![\p{L}\d])/giu;
const wordsOf = (ts: string[]) => new Set(ts.flatMap((t) => t.toLowerCase().split(/[-/]/)).map((w) => w.replace(/[^\p{L}%$"]/gu, '')).filter((w) => w && !STOP.has(w)));
// Marks and emphasis are not words of the claim; a unit written onto its number ("24U", "10am") is a word of its own.
const tokens = (text: string) => text.replace(/\[s\d+\]/g, ' ').replace(/[*_`]/g, '').replace(UNITS, '$1 $2').split(/[\s;:()[\]]+|,(?!\d)/).filter(Boolean);

/** Every number standing on its own (not inside a model code such as AR3104 or IP54), as shown without its thousands commas. */
export function figures(text: string): Figure[] {
  const ts = tokens(text);
  return ts.flatMap((t, i) => {
    const m = /^[~≈≥≤<>±+]*(\$?)(\d[\d,]*(?:\.\d+)?)\.?$/.exec(t);
    // A currency sign is the figure's unit as much as a word after it is.
    const unit = wordsOf([...ts.slice(i + 1, i + 3), m?.[1] ?? '']);
    return m ? [{ value: m[2].replace(/,/g, ''), unit, near: wordsOf([...ts.slice(Math.max(0, i - 3), i), ...ts.slice(i + 1, i + 4)]) }] : [];
  });
}

const decimals = (v: string) => v.split('.')[1]?.length ?? 0;
/** Whether two shown values are one value: the more precise rounds to the other, so 1,066.8 is the ~1,067 of another passage. */
export const sameValue = (a: string, b: string) => Math.abs(Number(a) - Number(b)) < 10 ** -Math.min(decimals(a), decimals(b)) / 2 + 1e-9;

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
