// sloplint for TypeScript: comment runs and cyclomatic complexity on diff-touched code, mirroring the Go tool.
// Usage: node tools/sloplint.ts diff <git-ref> | repo
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import ts from 'typescript';

export const MAX_COMMENT_RUN = 2;
export const MAX_CC = 15;
const ALLOW = 'sloplint: cc-allow';

type Range = [number, number];
export type Finding = { line: number; message: string };

const overlaps = (s: number, e: number, rs: Range[] | 'all') => rs === 'all' || rs.some(([a, b]) => s <= b && e >= a);

/** Changed line ranges per file (new-file numbers) from `git diff -U0` output. */
export function changedRanges(diff: string): Map<string, Range[]> {
  const out = new Map<string, Range[]>();
  let file = '';
  for (const line of diff.split('\n')) {
    if (line.startsWith('+++ ')) file = line === '+++ /dev/null' ? '' : line.slice(6);
    const hunk = /^@@ -\S+ \+(\d+)(?:,(\d+))? @@/.exec(line);
    if (!hunk || !file) continue;
    const start = Number(hunk[1]) || 1;
    const count = hunk[2] === undefined ? 1 : Number(hunk[2]);
    if (count > 0) out.set(file, [...(out.get(file) ?? []), [start, start + count - 1]]);
  }
  return out;
}

/** Runs of consecutive comment lines (a block comment counts each of its lines) longer than the limit. */
function commentRuns(sf: ts.SourceFile, rs: Range[] | 'all'): Finding[] {
  const lines = sf.text.split('\n');
  const isComment = new Array(lines.length).fill(false);
  ts.forEachChild(sf, function visit(node) {
    for (const r of ts.getLeadingCommentRanges(sf.text, node.getFullStart()) ?? []) {
      const s = sf.getLineAndCharacterOfPosition(r.pos).line;
      const e = sf.getLineAndCharacterOfPosition(r.end).line;
      for (let l = s; l <= e; l++) isComment[l] = true;
    }
    ts.forEachChild(node, visit);
  });
  const findings: Finding[] = [];
  for (let l = 0; l < lines.length; l++) {
    if (!isComment[l] || (l > 0 && isComment[l - 1])) continue;
    let end = l;
    while (end + 1 < lines.length && isComment[end + 1]) end++;
    const n = end - l + 1;
    if (n > MAX_COMMENT_RUN && overlaps(l + 1, end + 1, rs)) findings.push({ line: l + 1, message: `${n}-line comment run touches changed lines` });
  }
  return findings;
}

const DECISIONS = new Set([
  ts.SyntaxKind.IfStatement, ts.SyntaxKind.ConditionalExpression, ts.SyntaxKind.ForStatement, ts.SyntaxKind.ForInStatement,
  ts.SyntaxKind.ForOfStatement, ts.SyntaxKind.WhileStatement, ts.SyntaxKind.DoStatement, ts.SyntaxKind.CaseClause, ts.SyntaxKind.CatchClause,
]);
const LOGICAL = new Set([ts.SyntaxKind.AmpersandAmpersandToken, ts.SyntaxKind.BarBarToken, ts.SyntaxKind.QuestionQuestionToken]);

/** Cyclomatic complexity of one function body, not counting nested functions. */
export function complexity(fn: ts.FunctionLikeDeclaration): number {
  let cc = 1;
  const visit = (node: ts.Node): void => {
    if (node !== fn && ts.isFunctionLike(node)) return;
    if (DECISIONS.has(node.kind)) cc++;
    if (ts.isBinaryExpression(node) && LOGICAL.has(node.operatorToken.kind)) cc++;
    ts.forEachChild(node, visit);
  };
  if (fn.body) ts.forEachChild(fn.body, visit);
  return cc;
}

function allowed(sf: ts.SourceFile, fn: ts.Node): boolean {
  const top = sf.getLineAndCharacterOfPosition(fn.getStart(sf)).line;
  const lines = sf.text.split('\n').slice(Math.max(0, top - 3), top);
  return lines.some((l) => {
    const i = l.indexOf(ALLOW);
    return l.trim().startsWith('//') && i >= 0 && l.slice(i + ALLOW.length).trim().length > 0;
  });
}

const fnName = (fn: ts.FunctionLikeDeclaration) => (fn.name && ts.isIdentifier(fn.name) ? fn.name.text : '<anonymous>');

function complexFunctions(sf: ts.SourceFile, rs: Range[] | 'all'): Finding[] {
  const findings: Finding[] = [];
  const visit = (node: ts.Node): void => {
    if (ts.isFunctionLike(node) && 'body' in node && node.body) {
      const fn = node as ts.FunctionLikeDeclaration;
      const s = sf.getLineAndCharacterOfPosition(fn.body!.getStart(sf)).line + 1;
      const e = sf.getLineAndCharacterOfPosition(fn.end).line + 1;
      const cc = complexity(fn);
      if (cc > MAX_CC && overlaps(s, e, rs) && !allowed(sf, fn)) findings.push({ line: s, message: `${fnName(fn)} CC ${cc} > ${MAX_CC} (changed)` });
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
  return findings;
}

export function lintSource(path: string, text: string, rs: Range[] | 'all'): Finding[] {
  const sf = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true);
  return [...commentRuns(sf, rs), ...complexFunctions(sf, rs)];
}

const linted = (rel: string) => /\.tsx?$/.test(rel) && !/\.test\.tsx?$|\/test\/|\/fixtures\/|\.d\.ts$|node_modules\//.test(rel);

function run(mode: string, ref: string | undefined): number {
  const root = execFileSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' }).trim();
  const files = mode === 'repo'
    ? new Map(execFileSync('git', ['ls-files', 'v1'], { cwd: root, encoding: 'utf8' }).split('\n').map((f) => [f, 'all' as const]))
    : changedRanges(execFileSync('git', ['diff', '-U0', `${ref}...HEAD`, '--', 'v1'], { cwd: root, encoding: 'utf8', maxBuffer: 1 << 28 }));
  let count = 0;
  for (const [rel, rs] of files) {
    // A file the diff names but the working tree no longer has (moved or deleted) has nothing left to lint.
    if (!linted(rel) || !existsSync(join(root, rel))) continue;
    for (const f of lintSource(rel, readFileSync(join(root, rel), 'utf8'), rs)) {
      console.log(`${relative(process.cwd(), join(root, rel))}:${f.line}: ${f.message}`);
      count++;
    }
  }
  if (count === 0) console.log('sloplint: ok');
  return mode === 'repo' ? 0 : Math.min(count, 1);
}

if (import.meta.main) {
  const [mode, ref] = process.argv.slice(2);
  if (mode !== 'repo' && !(mode === 'diff' && ref)) {
    console.error('usage: sloplint <diff <git-ref> | repo>');
    process.exit(2);
  }
  process.exit(run(mode, ref));
}
