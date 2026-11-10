// Detector D: break code the test ran (V8 coverage; vitest: the names it imports) and check the test fails.
// Usage: node tools/flake-spike/mutate.ts [prs|known] [concurrency] [runner]  (writes results/mutate[-set].json)
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, normalize, relative } from 'node:path';
import { fileURLToPath } from 'node:url';
import ts from 'typescript';
import type { Case } from './cases.ts';
import { ROOT, runCase } from './run.ts';
import { caseSet, parse } from './static.ts';

const MUTANTS = 8;
const HOOK = `--import=${join(ROOT, 'tools/flake-spike/mutant-hook.ts')}`;
type Range = { startOffset: number; endOffset: number; count: number };
type Mutant = { file: string; at: number; desc: string; apply: (src: string) => string };

/** The source files a test file imports by relative path, following one level of re-exports from an index. */
export function subjects(test: string): string[] {
  const out = new Set<string>();
  const add = (from: string, spec: string) => {
    const p = normalize(join(dirname(from), spec));
    if (!/\.test\.tsx?$/.test(p) && /\.tsx?$/.test(p)) out.add(p);
  };
  for (const s of parse(test).statements) if (ts.isImportDeclaration(s) && (s.moduleSpecifier as ts.StringLiteral).text.startsWith('.')) add(test, (s.moduleSpecifier as ts.StringLiteral).text);
  for (const f of [...out].filter((p) => p.endsWith('index.ts'))) {
    for (const s of parse(f).statements) if (ts.isExportDeclaration(s) && s.moduleSpecifier && (s.moduleSpecifier as ts.StringLiteral).text.startsWith('.')) add(f, (s.moduleSpecifier as ts.StringLiteral).text);
  }
  return [...out].filter((p) => !/fixtures|fake|harness|test-/.test(p));
}

/** Per repo file, the block ranges V8 recorded, merged over every process the run started. */
function coverage(dir: string): Map<string, Range[]> {
  const out = new Map<string, Range[]>();
  for (const f of readdirSync(dir)) {
    for (const s of JSON.parse(readFileSync(join(dir, f), 'utf8')).result as Array<{ url: string; functions: Array<{ ranges: Range[] }> }>) {
      if (!s.url.startsWith('file://')) continue;
      const path = relative(ROOT, fileURLToPath(s.url));
      out.set(path, [...(out.get(path) ?? []), ...s.functions.flatMap((fn) => fn.ranges)]);
    }
  }
  return out;
}

// Executed when some process ran it: the innermost range around the offset has a nonzero count.
const ran = (ranges: Range[], at: number) => {
  const around = ranges.filter((r) => r.startOffset <= at && at < r.endOffset).sort((a, b) => a.endOffset - a.startOffset - (b.endOffset - b.startOffset));
  return !!around[0] && around[0].count > 0;
};

const splice = (start: number, end: number, text: string) => (src: string) => src.slice(0, start) + text + src.slice(end);
const FLIP: Partial<Record<ts.SyntaxKind, string>> = {
  [ts.SyntaxKind.EqualsEqualsEqualsToken]: '!==', [ts.SyntaxKind.ExclamationEqualsEqualsToken]: '===', [ts.SyntaxKind.LessThanToken]: '>=',
  [ts.SyntaxKind.GreaterThanToken]: '<=', [ts.SyntaxKind.LessThanEqualsToken]: '>', [ts.SyntaxKind.GreaterThanEqualsToken]: '<',
  [ts.SyntaxKind.AmpersandAmpersandToken]: '||', [ts.SyntaxKind.BarBarToken]: '&&',
};

function mutantAt(n: ts.Node, file: string): Mutant | undefined {
  const at = n.getStart();
  const line = n.getSourceFile().getLineAndCharacterOfPosition(at).line + 1;
  const m = (desc: string, apply: (s: string) => string): Mutant => ({ file, at, desc: `${file}:${line} ${desc}`, apply });
  if (ts.isIfStatement(n)) return m(`if (!(${n.expression.getText().slice(0, 40)}))`, (s) => splice(n.expression.getStart(), n.expression.end, `!(${n.expression.getText()})`)(s));
  if (ts.isReturnStatement(n) && n.expression) return m(`return undefined (was ${n.expression.getText().slice(0, 40)})`, splice(n.expression.getStart(), n.expression.end, 'undefined'));
  if (ts.isExpressionStatement(n) && (ts.isCallExpression(n.expression) || ts.isAwaitExpression(n.expression))) return m(`drop ${n.getText().slice(0, 50)}`, splice(at, n.end, 'void 0;'));
  if (ts.isBinaryExpression(n) && FLIP[n.operatorToken.kind]) return m(`${n.operatorToken.getText()} -> ${FLIP[n.operatorToken.kind]} in ${n.getText().slice(0, 40)}`, splice(n.operatorToken.getStart(), n.operatorToken.end, FLIP[n.operatorToken.kind]!));
  if (n.kind === ts.SyntaxKind.TrueKeyword || n.kind === ts.SyntaxKind.FalseKeyword) return m(`flip ${n.getText()}`, splice(at, n.end, n.kind === ts.SyntaxKind.TrueKeyword ? 'false' : 'true'));
  return undefined;
}

/** The names a test imports from its relative (in-repo) modules. */
const importedNames = (test: string) => new Set(parse(test).statements.filter(ts.isImportDeclaration).filter((d) => (d.moduleSpecifier as ts.StringLiteral).text.startsWith('.'))
  .flatMap((d) => [...((d.importClause?.namedBindings as ts.NamedImports | undefined)?.elements ?? [])].map((e) => (e.propertyName ?? e.name).text).concat(d.importClause?.name ? ['default'] : [])));

const declares = (s: ts.Statement, only: Set<string>) => {
  if ((ts.isFunctionDeclaration(s) || ts.isClassDeclaration(s)) && s.name) return only.has(s.name.text) || (only.has('default') && /^export default/.test(s.getText()));
  return ts.isVariableStatement(s) && s.declarationList.declarations.some((d) => only.has(d.name.getText()));
};

function candidates(file: string, executed?: Range[], only?: Set<string>): Mutant[] {
  const out: Mutant[] = [];
  const visit = (n: ts.Node) => {
    if (ts.isTypeNode(n) || ts.isImportDeclaration(n) || ts.isInterfaceDeclaration(n) || ts.isTypeAliasDeclaration(n)) return;
    // Module-level code runs at import for every test, so a mutant there says nothing about this one; only function bodies.
    const mutant = ts.findAncestor(n.parent, ts.isFunctionLike) ? mutantAt(n, file) : undefined;
    if (mutant && (!executed || ran(executed, mutant.at))) out.push(mutant);
    n.forEachChild(visit);
  };
  const sf = parse(file);
  for (const s of only ? sf.statements.filter((x) => declares(x, only)) : [sf]) visit(s);
  return out;
}

// A seeded shuffle, so a rerun picks the same mutants.
export function sample<T>(xs: T[], seed: string, n: number): T[] {
  let h = [...seed].reduce((a, ch) => Math.imul(a ^ ch.charCodeAt(0), 16777619), 2166136261) >>> 0;
  const rnd = () => ((h = Math.imul(h ^ (h >>> 15), 2246822507) >>> 0), h / 2 ** 32);
  return xs.map((x) => [rnd(), x] as const).sort((a, b) => a[0] - b[0]).slice(0, n).map(([, x]) => x);
}

export type MutateResult = { id: string; status: 'ok' | 'baseline-fail' | 'no-subject' | 'no-mutants'; subjects: string[]; mutants: number; killed: number; survived: string[]; ms: number; runs: number };

export async function mutate(c: Case): Promise<MutateResult> {
  const started = Date.now();
  const tmp = mkdtempSync(join(tmpdir(), 'flake-mutate-'));
  const base = { id: c.id, subjects: [] as string[], mutants: 0, killed: 0, survived: [] as string[], runs: 1 };
  try {
    const subj = c.subjects.length ? c.subjects : subjects(c.file);
    const cov = join(tmp, 'cov');
    const baseline = await runCase(c, { env: c.runner === 'node' ? { NODE_V8_COVERAGE: cov } : {}, timeoutMs: 120_000 });
    if (!baseline.pass || !baseline.ran) return { ...base, status: 'baseline-fail', ms: Date.now() - started };
    if (!subj.length) return { ...base, status: 'no-subject', ms: Date.now() - started };
    const executed = c.runner === 'node' ? coverage(cov) : undefined;
    // Without coverage, only the declarations the test imports by name are mutated, not the rest of a big module.
    const only = executed ? undefined : importedNames(c.file);
    const pool = subj.flatMap((f) => candidates(f, executed ? executed.get(f) ?? [] : undefined, only));
    const chosen = sample(pool, c.id, MUTANTS);
    const result = { ...base, subjects: subj, mutants: chosen.length, runs: 1 + chosen.length };
    if (!chosen.length) return { ...result, status: 'no-mutants', ms: Date.now() - started };
    for (const [i, m] of chosen.entries()) {
      const src = join(tmp, `m${i}.ts`);
      writeFileSync(src, m.apply(readFileSync(join(ROOT, m.file), 'utf8')));
      const env = { FLAKE_MUTANT_FILE: join(ROOT, m.file), FLAKE_MUTANT_SRC: src, ...(c.runner === 'node' && { NODE_OPTIONS: HOOK }) };
      const r = await runCase(c, { env, timeoutMs: Math.max(5 * baseline.ms, 20_000) });
      if (r.pass && r.ran) result.survived.push(m.desc);
      else result.killed++;
    }
    return { ...result, status: 'ok', ms: Date.now() - started };
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
}

/** PR cases run at HEAD, under the path the file has there, with subjects found from the HEAD imports. */
export const atHead = (cases: Array<Case & { head?: string }>) => cases.filter((c) => c.runnable).map((c) => (c.head ? { ...c, file: c.head, rev: undefined, subjects: [] } : c));

if (import.meta.main) {
  const { cases, out } = caseSet(process.argv[2]);
  // An optional runner reruns only that runner's cases, replacing their entries in the existing results.
  const runner = process.argv[4];
  const list = atHead(cases).filter((c) => !runner || c.runner === runner);
  const file = join(ROOT, `tools/flake-spike/results/mutate${out}.json`);
  const prior = runner ? JSON.parse(readFileSync(file, 'utf8')) : { ms: 0, results: [] };
  const kept: MutateResult[] = prior.results.filter((r: MutateResult) => !list.some((c) => c.id === r.id));
  const results: MutateResult[] = [];
  let next = 0;
  const worker = async () => {
    while (next < list.length) {
      const c = list[next++];
      const r = await mutate(c);
      results.push(r);
      console.log(`${results.length}/${list.length} ${r.id} ${r.status} killed ${r.killed}/${r.mutants} ${r.ms}ms`);
    }
  };
  const started = Date.now();
  await Promise.all(Array.from({ length: Number(process.argv[3] ?? 16) }, worker));
  mkdirSync(join(ROOT, 'tools/flake-spike/results'), { recursive: true });
  writeFileSync(file, `${JSON.stringify({ ms: prior.ms + Date.now() - started, results: [...kept, ...results] }, null, 1)}\n`);
}
