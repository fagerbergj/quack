// Detector A: a TypeScript AST scan of one test (its body, the file's helpers and hooks) and the code it exercises.
// Usage: node tools/flake-spike/static.ts  (writes tools/flake-spike/results/static.json)
import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import ts from 'typescript';
import { CASES, type Case, type Category } from './cases.ts';
import { ROOT } from './run.ts';
import { RULES } from './static-rules.ts';

export type Hit = { rule: string; category: Exclude<Category, 'good'>; cls: string; at: string };
type Assertion = { node: ts.CallExpression; matcher: string; actual?: ts.Expression; expected?: ts.Expression };
export type Ctx = { sf: ts.SourceFile; body: ts.Node; file: ts.Node[]; subjects: ts.SourceFile[]; asserts: Assertion[]; mocks: Set<string>; fakeTimers: boolean; helperAsserts: boolean };

// A case with a rev is read from that commit, as the PR merged it.
export const source = (path: string, rev?: string) => (rev ? execFileSync('git', ['show', `${rev}:${path}`], { cwd: ROOT, encoding: 'utf8', maxBuffer: 1 << 26 }) : readFileSync(join(ROOT, path), 'utf8'));
export const parse = (path: string, rev?: string) => ts.createSourceFile(path, source(path, rev), ts.ScriptTarget.Latest, true, /\.tsx/.test(path) ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
const text = (n: ts.Node) => n.getText().replace(/\s+/g, ' ');

function walk(n: ts.Node, f: (n: ts.Node) => void): void {
  f(n);
  n.forEachChild((c) => walk(c, f));
}

function all<T extends ts.Node>(roots: ts.Node[], is: (n: ts.Node) => n is T): T[] {
  const out: T[] = [];
  for (const r of roots) walk(r, (n) => void (is(n) && out.push(n)));
  return out;
}

const calls = (roots: ts.Node[]) => all(roots, ts.isCallExpression);
const callee = (c: ts.CallExpression) => text(c.expression);
const isTestCall = (n: ts.Node): n is ts.CallExpression => ts.isCallExpression(n) && /^(test|it)(\.each\(.*\))?$/.test(callee(n).replace(/\.(only|skip|concurrent)$/, ''));
const testName = (c: ts.CallExpression) => (c.arguments[0] && ts.isStringLiteralLike(c.arguments[0]) ? c.arguments[0].text : undefined);

/** The named test's call, or the whole file; the file's other top-level statements are its helpers and hooks. */
function scope(sf: ts.SourceFile, name?: string): { body: ts.Node; file: ts.Node[] } {
  const tests = all([sf], isTestCall);
  const own = name ? tests.find((t) => testName(t) === name) : undefined;
  if (name && !own) throw new Error(`${sf.fileName}: no test named ${name}`);
  return { body: own ?? sf, file: sf.statements.filter((s) => !tests.some((t) => s.pos <= t.pos && t.end <= s.end)) };
}

function assertionOf(c: ts.CallExpression): Assertion | undefined {
  const name = callee(c);
  if (/^(assert|t\.assert)(\.\w+)?$/.test(name) && !/\.(fail)$/.test(name)) return { node: c, matcher: name.split('.').at(-1) === 'assert' ? 'ok' : name.split('.').at(-1)!, actual: c.arguments[0], expected: c.arguments[1] };
  if (name !== 'expect' && name !== 'expect.soft') return undefined;
  let top: ts.Node = c;
  while (ts.isPropertyAccessExpression(top.parent) || (ts.isCallExpression(top.parent) && top.parent.expression === top)) top = top.parent;
  if (!ts.isCallExpression(top)) return { node: c, matcher: 'none', actual: c.arguments[0] };
  return { node: top, matcher: text(top.expression).split('.').slice(1).join('.') || 'none', actual: c.arguments[0], expected: top.arguments[0] };
}

const isMockInit = (e: ts.Expression | undefined) => !!e && /^(vi|jest|mock|t\.mock)\.(fn|spyOn|method)\(|^vi\.fn\(\)\./.test(text(e));

function context(c: Case): Ctx {
  const sf = parse(c.file, c.rev);
  const { body, file } = scope(sf, c.name);
  const helpers = file.filter((s) => ts.isFunctionDeclaration(s) || ts.isVariableStatement(s));
  const mocks = new Set(all([body, ...file], ts.isVariableDeclaration).filter((d) => isMockInit(d.initializer)).map((d) => d.name.getText()));
  return {
    sf, body, file, mocks,
    subjects: c.subjects.map((p) => parse(p, c.rev)),
    asserts: calls([body]).map(assertionOf).filter((a) => !!a),
    fakeTimers: /useFakeTimers|mock\.timers\.enable|FakeClock|fakeClock/.test(sf.text),
    helperAsserts: calls(helpers).some((x) => !!assertionOf(x)),
  };
}

export { all, calls, callee, context, text, walk, type Assertion };

/** Every rule that fires on the case. */
export function scan(c: Case): Hit[] {
  const ctx = context(c);
  return RULES.flatMap((r) => {
    const at = r.check(ctx);
    return at ? [{ rule: r.id, category: r.category, cls: r.cls, at: at.slice(0, 200) }] : [];
  });
}

/** The corpus, or with `prs` the tests touched by recent PRs (results/pr-cases.json from prs.ts). */
export const caseSet = (set?: string): { cases: Case[]; out: string } =>
  set === 'prs' || set === 'known'
    ? { cases: JSON.parse(readFileSync(join(ROOT, `tools/flake-spike/results/${set === 'prs' ? 'pr' : 'known'}-cases.json`), 'utf8')), out: `-${set}` }
    : { cases: CASES, out: '' };

if (import.meta.main) {
  const { cases, out } = caseSet(process.argv[2]);
  const started = performance.now();
  const results = [];
  for (const c of cases) {
    const t = performance.now();
    results.push({ id: c.id, hits: scan(c), ms: Math.round(performance.now() - t) });
  }
  mkdirSync(join(ROOT, 'tools/flake-spike/results'), { recursive: true });
  writeFileSync(join(ROOT, `tools/flake-spike/results/static${out}.json`), `${JSON.stringify({ ms: Math.round(performance.now() - started), results }, null, 1)}\n`);
  for (const r of results) if (r.hits.length) console.log(r.id, r.hits.map((h) => h.rule).join(' '));
}
