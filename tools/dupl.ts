// Duplicated-code detection over token windows.
import ts from 'typescript';

export const MIN_TOKENS = 80;

type Range = [number, number];
type Tok = { id: number; line: number };
type Clone = { file: string; line: number; endLine: number; other: string; otherLine: number };

const NAME = new Set([ts.SyntaxKind.Identifier, ts.SyntaxKind.PrivateIdentifier]);
const LITERAL = new Set([ts.SyntaxKind.StringLiteral, ts.SyntaxKind.NumericLiteral, ts.SyntaxKind.BigIntLiteral, ts.SyntaxKind.NoSubstitutionTemplateLiteral,
  ts.SyntaxKind.TemplateHead, ts.SyntaxKind.TemplateMiddle, ts.SyntaxKind.TemplateTail, ts.SyntaxKind.RegularExpressionLiteral, ts.SyntaxKind.JsxText]);
const SKIPPED = new Set([ts.SyntaxKind.ImportDeclaration, ts.SyntaxKind.ExportDeclaration, ts.SyntaxKind.EndOfFileToken]);

// Names are erased, so a copy with renamed variables still matches; literals stay, or every rubric table would match every other.
// Imports are skipped, being alike by nature.
function tokens(path: string, text: string, vocab: Map<string, number>): Tok[] {
  const sf = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true, path.endsWith('x') ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
  const out: Tok[] = [];
  const visit = (node: ts.Node): void => {
    if (SKIPPED.has(node.kind)) return;
    const children = node.getChildren(sf);
    if (children.length) return children.forEach(visit);
    const key = NAME.has(node.kind) ? '$name' : LITERAL.has(node.kind) ? node.getText(sf) : ts.SyntaxKind[node.kind];
    if (!vocab.has(key)) vocab.set(key, vocab.size);
    out.push({ id: vocab.get(key)!, line: sf.getLineAndCharacterOfPosition(node.getStart(sf)).line + 1 });
  };
  visit(sf);
  return out;
}

// Two independent 32-bit FNV-1a hashes of the window: 64 bits keep collisions out of reach without holding each window's text.
function windowKey(toks: Tok[], at: number): string {
  let [a, b] = [0x811c9dc5, 0x01000193];
  for (let i = at; i < at + MIN_TOKENS; i++) {
    a = Math.imul(a ^ toks[i].id, 0x01000193);
    b = Math.imul(b ^ (toks[i].id + 0x9e3779b9), 0x01000193) ^ (b >>> 15);
  }
  return `${a >>> 0}:${b >>> 0}`;
}
const touches = (s: number, e: number, rs: Range[] | 'all') => rs === 'all' || rs.some(([a, b]) => s <= b && e >= a);

/** The window's smallest period: the least p with toks[i] === toks[i + p] across it, from the KMP failure function. */
function period(toks: Tok[], at: number): number {
  const fail = new Array<number>(MIN_TOKENS).fill(0);
  for (let i = 1; i < MIN_TOKENS; i++) {
    let k = fail[i - 1];
    while (k > 0 && toks[at + i].id !== toks[at + k].id) k = fail[k - 1];
    fail[i] = toks[at + i].id === toks[at + k].id ? k + 1 : k;
  }
  return MIN_TOKENS - fail[MIN_TOKENS - 1];
}

type Run = { p: number; start: number; end: number };

/** The maximal stretch [start, end) around the window that repeats with the window's period; reuses `prev` while inside it. */
function runAt(toks: Tok[], at: number, prev: Run | undefined): Run {
  const p = period(toks, at);
  if (prev && prev.p === p && at >= prev.start && at + MIN_TOKENS <= prev.end) return prev;
  let [start, end] = [at, at + MIN_TOKENS];
  while (start > 0 && toks[start - 1].id === toks[start - 1 + p].id) start--;
  while (end < toks.length && toks[end].id === toks[end - p].id) end++;
  return { p, start, end };
}

/** Runs of at least MIN_TOKENS tokens on changed lines that also occur elsewhere in `sources`, one finding per run. */
export function clones(sources: Map<string, string>, changed: Map<string, Range[] | 'all'>): Clone[] {
  const vocab = new Map<string, number>();
  const toks = new Map([...sources].map(([f, text]) => [f, tokens(f, text, vocab)]));
  const seen = new Map<string, Array<{ file: string; at: number }>>();
  for (const [file, list] of toks) for (let at = 0; at + MIN_TOKENS <= list.length; at++) {
    const key = windowKey(list, at);
    const hits = seen.get(key);
    if (hits) hits.push({ file, at });
    else seen.set(key, [{ file, at }]);
  }
  const found: Clone[] = [];
  for (const [file, rs] of changed) {
    const mine = toks.get(file);
    if (!mine) continue;
    let open: Clone | undefined;
    let run: Run | undefined;
    for (let at = 0; at + MIN_TOKENS <= mine.length; at++) {
      const [start, end] = [mine[at].line, mine[at + MIN_TOKENS - 1].line];
      const hits = touches(start, end, rs) ? seen.get(windowKey(mine, at))! : [];
      if (hits.some((o) => o.file === file && o.at !== at)) run = runAt(mine, at, run);
      // An occurrence overlapping this one, or another phase of one run that repeats a shorter unit, is not a copy.
      const self = (o: { at: number }) => Math.abs(o.at - at) < MIN_TOKENS || (run!.p < MIN_TOKENS && Math.min(o.at, at) >= run!.start && Math.max(o.at, at) + MIN_TOKENS <= run!.end);
      const other = hits.find((o) => o.file !== file) ?? hits.find((o) => o.file === file && !self(o));
      if (!other) open = undefined;
      else if (open && end >= open.endLine) open.endLine = end;
      else found.push((open = { file, line: start, endLine: end, other: other.file, otherLine: toks.get(other.file)![other.at].line }));
    }
  }
  return found;
}
