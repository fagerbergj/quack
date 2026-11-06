// Duplicated-code detection over token windows, the TypeScript counterpart of v0's golangci dupl gate.
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
    for (let at = 0; at + MIN_TOKENS <= mine.length; at++) {
      const [start, end] = [mine[at].line, mine[at + MIN_TOKENS - 1].line];
      // A window overlapping itself (a repeated run inside one stretch) is not a copy.
      const other = touches(start, end, rs) ? seen.get(windowKey(mine, at))!.find((o) => o.file !== file || Math.abs(o.at - at) >= MIN_TOKENS) : undefined;
      if (!other) open = undefined;
      else if (open && end >= open.endLine) open.endLine = end;
      else found.push((open = { file, line: start, endLine: end, other: other.file, otherLine: toks.get(other.file)![other.at].line }));
    }
  }
  return found;
}
