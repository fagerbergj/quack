// The tests each PR merged since a date added or changed, read at its merge commit: one case per touched test.
// Usage: node tools/flake-spike/prs.ts 2026-11-03  (writes tools/flake-spike/results/pr-cases.json)
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join, normalize } from 'node:path';
import ts from 'typescript';
import { changedRanges } from '../sloplint.ts';
import type { Case } from './cases.ts';
import { ROOT } from './run.ts';
import { all, callee, parse } from './static.ts';

const git = (...args: string[]) => execFileSync('git', args, { cwd: ROOT, encoding: 'utf8', maxBuffer: 1 << 28 });
const lineOf = (sf: ts.SourceFile, pos: number) => sf.getLineAndCharacterOfPosition(pos).line + 1;
const isTest = (n: ts.Node): n is ts.CallExpression => ts.isCallExpression(n) && /^(test|it)(\.(only|skip|todo))?$/.test(callee(n)) && !!n.arguments[0] && ts.isStringLiteralLike(n.arguments[0]);
const nameOf = (c: ts.CallExpression) => (c.arguments[0] as ts.StringLiteral).text;

/** The non-test files a test file imports by relative path, as they were at rev. */
function subjectsOf(sf: ts.SourceFile, rev: string): string[] {
  const exists = (p: string) => { try { git('cat-file', '-e', `${rev}:${p}`); return true; } catch { return false; } };
  return sf.statements.filter(ts.isImportDeclaration).map((d) => (d.moduleSpecifier as ts.StringLiteral).text).filter((s) => s.startsWith('.'))
    .map((s) => normalize(join(dirname(sf.fileName), s))).filter((p) => !/\.test\.tsx?$|fixtures|fake|harness|helpers/.test(p) && /\.tsx?$/.test(p) && exists(p));
}

type PrCase = Case & { pr: number; head?: string; line: number };

function casesOf(pr: number, sha: string): PrCase[] {
  try {
    git('cat-file', '-e', `${sha}^`);
  } catch {
    // A PR merged into a stacked base branch: its merge commit is not on main's history here.
    git('fetch', '-q', 'origin', sha);
  }
  const ranges = changedRanges(git('diff', '-U0', '--find-renames', `${sha}^`, sha, '--', '*.test.ts', '*.test.tsx'));
  return [...ranges].flatMap(([file, rs]) => {
    const sf = parse(file, sha);
    const touched = all([sf], isTest).filter((t) => rs.some(([a, b]) => lineOf(sf, t.getStart()) <= b && lineOf(sf, t.end) >= a));
    const head = file.replace(/^v1\//, '');
    return touched.map((t) => ({
      id: `${pr}:${head}:${lineOf(sf, t.getStart())}`, pr, line: lineOf(sf, t.getStart()), category: 'good' as const, cls: 'unlabeled', file, name: nameOf(t),
      subjects: subjectsOf(sf, sha), runner: head.startsWith('packages/web/') ? 'vitest-web' as const : 'node' as const, runnable: false, source: `#${pr}`, rev: sha, head,
    }));
  });
}

if (import.meta.main) {
  const since = process.argv[2] ?? '2026-11-03';
  const prs = JSON.parse(execFileSync('gh', ['pr', 'list', '-R', 'fagerbergj/quack', '--state', 'merged', '--search', `merged:>=${since}`, '--limit', '500', '--json', 'number,mergeCommit,mergedAt'], { encoding: 'utf8' })) as Array<{ number: number; mergeCommit: { oid: string }; mergedAt: string }>;
  prs.sort((a, b) => a.mergedAt.localeCompare(b.mergedAt));
  const latest = new Map<string, PrCase>();
  for (const p of prs) for (const c of casesOf(p.number, p.mergeCommit.oid)) latest.set(`${c.head}\0${c.name}`, c);
  // Runnable when the same test still exists at HEAD, where mutation and repeat runs happen.
  const out = [...latest.values()].map((c) => ({ ...c, runnable: existsSync(join(ROOT, c.head!)) && all([parse(c.head!)], isTest).some((t) => nameOf(t) === c.name) }));
  mkdirSync(join(ROOT, 'tools/flake-spike/results'), { recursive: true });
  writeFileSync(join(ROOT, 'tools/flake-spike/results/pr-cases.json'), `${JSON.stringify(out, null, 1)}\n`);
  console.log(`${prs.length} PRs, ${out.length} touched tests (${out.filter((c) => c.runnable).length} still at HEAD) in ${new Set(out.map((c) => c.head)).size} files`);
}
