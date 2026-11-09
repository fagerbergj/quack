export type Line = { kind: 'same' | 'add' | 'del' | 'gap'; text: string };

const CONTEXT = 2;

// The LCS table is O(n*m); prompts are hundreds of lines, so a library would only add weight.
function lcs(a: string[], b: string[]): Line[] {
  const t = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i--) for (let j = b.length - 1; j >= 0; j--) t[i]![j] = a[i] === b[j] ? t[i + 1]![j + 1]! + 1 : Math.max(t[i + 1]![j]!, t[i]![j + 1]!);
  const out: Line[] = [];
  let [i, j] = [0, 0];
  while (i < a.length || j < b.length) {
    if (i < a.length && j < b.length && a[i] === b[j]) out.push({ kind: 'same', text: a[i++]! }), j++;
    else if (i < a.length && (j === b.length || t[i + 1]![j]! >= t[i]![j + 1]!)) out.push({ kind: 'del', text: a[i++]! });
    else out.push({ kind: 'add', text: b[j++]! });
  }
  return out;
}

export function lineDiff(before: string, after: string): Line[] {
  const [a, b] = [before, after].map((t) => (t.trimEnd() ? t.trimEnd().split('\n') : [])) as [string[], string[]];
  return trim(lcs(a, b));
}

// Keeps CONTEXT unchanged lines around each change and folds the rest into one gap; no changes yields [].
function trim(lines: Line[]): Line[] {
  if (lines.every((l) => l.kind === 'same')) return [];
  const near = (i: number) => lines.slice(Math.max(0, i - CONTEXT), i + CONTEXT + 1).some((n) => n.kind !== 'same');
  const out: Line[] = [];
  lines.forEach((l, i) => {
    if (near(i)) out.push(l);
    else if (out.at(-1)?.kind !== 'gap') out.push({ kind: 'gap', text: '' });
  });
  return out;
}

/** Unchanged words kept on each side of a change before the rest folds into a gap, whose text is the folded words. */
export const WORDS = 6;
const tokens = (s: string) => s.match(/\s+|\S+/g) ?? [];

// Whitespace kept between two changes joins them, so a replaced phrase reads as one struck run then one added run.
function hunks(ops: Line[]): Line[] {
  const changed = (o?: Line) => !!o && o.kind !== 'same';
  const out: Line[] = [];
  let del = '', add = '';
  const flush = () => { if (del) out.push({ kind: 'del', text: del }); if (add) out.push({ kind: 'add', text: add }); del = add = ''; };
  ops.forEach((o, i) => {
    const glue = o.kind === 'same' && !o.text.trim() && changed(ops[i - 1]) && changed(ops[i + 1]);
    if (o.kind === 'same' && !glue) {
      flush();
      if (out.at(-1)?.kind === 'same') out.at(-1)!.text += o.text;
      else out.push({ ...o });
    } else {
      if (o.kind !== 'add') del += o.text;
      if (o.kind !== 'del') add += o.text;
    }
  });
  flush();
  return out;
}

// An unchanged run keeps WORDS words next to each change it touches; folding fewer than three words saves nothing.
function fold(text: string, keepHead: boolean, keepTail: boolean): Line[] {
  const t = tokens(text);
  const at = t.flatMap((s, i) => (s.trim() ? [i] : []));
  const head = keepHead ? WORDS : 0, tail = keepTail ? WORDS : 0;
  if (at.length < head + tail + 3) return [{ kind: 'same', text }];
  const [a, b] = [head ? at[head]! : 0, tail ? at[at.length - tail]! : t.length];
  return ([{ kind: 'same', text: t.slice(0, a).join('') }, { kind: 'gap', text: t.slice(a, b).join('') }, { kind: 'same', text: t.slice(b).join('') }] as Line[]).filter((p) => p.text);
}

/** A word-level diff of two texts: struck and added runs, unchanged runs with long stretches folded into gaps; equal texts yield []. */
export function wordDiff(before: string, after: string): Line[] {
  if (before === after) return [];
  const runs = hunks(lcs(tokens(before), tokens(after)));
  return runs.flatMap((r, i) => (r.kind === 'same' ? fold(r.text, i > 0, i < runs.length - 1) : [r]));
}
