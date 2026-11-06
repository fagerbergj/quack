export type Line = { kind: 'same' | 'add' | 'del' | 'gap'; text: string };

const CONTEXT = 2;

// The LCS table is O(n*m); prompts are hundreds of lines, so a library would only add weight.
export function lineDiff(before: string, after: string): Line[] {
  const [a, b] = [before, after].map((t) => (t.trimEnd() ? t.trimEnd().split('\n') : [])) as [string[], string[]];
  const lcs = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i--) for (let j = b.length - 1; j >= 0; j--) lcs[i]![j] = a[i] === b[j] ? lcs[i + 1]![j + 1]! + 1 : Math.max(lcs[i + 1]![j]!, lcs[i]![j + 1]!);
  const out: Line[] = [];
  let [i, j] = [0, 0];
  while (i < a.length || j < b.length) {
    if (i < a.length && j < b.length && a[i] === b[j]) out.push({ kind: 'same', text: a[i++]! }), j++;
    else if (i < a.length && (j === b.length || lcs[i + 1]![j]! >= lcs[i]![j + 1]!)) out.push({ kind: 'del', text: a[i++]! });
    else out.push({ kind: 'add', text: b[j++]! });
  }
  return trim(out);
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
