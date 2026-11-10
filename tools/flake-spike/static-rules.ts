// Detector A's rules: each returns the offending code, or undefined. RULES in cases.ts names the book rule per class.
import { dirname, join, normalize } from 'node:path';
import ts from 'typescript';
import type { Category } from './cases.ts';
import { all, calls, callee, text, walk, type Assertion, type Ctx } from './static.ts';

type Rule = { id: string; category: Exclude<Category, 'good'>; cls: string; check: (c: Ctx) => string | undefined };

const scoped = (c: Ctx) => [c.body, ...c.file];
const loc = (n: ts.Node) => `${n.getSourceFile().fileName}:${n.getSourceFile().getLineAndCharacterOfPosition(n.getStart()).line + 1} ${text(n)}`;
const first = <T extends ts.Node>(xs: T[]) => (xs[0] ? loc(xs[0]) : undefined);
const last = (c: ts.CallExpression) => callee(c).split('.').at(-1) ?? '';
const num = (e: ts.Expression | undefined) => (e && ts.isNumericLiteral(e) ? Number(e.text.replace(/_/g, '')) : undefined);
const unwrap = (e: ts.Expression | undefined): ts.Expression | undefined => (e && (ts.isAwaitExpression(e) || ts.isParenthesizedExpression(e) || ts.isNonNullExpression(e)) ? unwrap(e.expression) : e);
const rootOf = (e: ts.Expression | undefined): string | undefined => {
  const u = unwrap(e);
  if (!u) return undefined;
  if (ts.isIdentifier(u)) return u.text;
  return ts.isPropertyAccessExpression(u) || ts.isElementAccessExpression(u) ? rootOf(u.expression) : undefined;
};
const decls = (c: Ctx) => all(scoped(c), ts.isVariableDeclaration);
const initOf = (c: Ctx, name: string) => decls(c).find((d) => d.name.getText() === name)?.initializer;
const isLiteral = (e: ts.Expression | undefined) => !!e && (ts.isLiteralExpression(e) || [ts.SyntaxKind.TrueKeyword, ts.SyntaxKind.FalseKeyword, ts.SyntaxKind.NullKeyword].includes(e.kind) || (ts.isIdentifier(e) && e.text === 'undefined'));

// Real time: sleeps and wall-clock reads, unless the file installs a fake clock.
function realSleep(c: Ctx) {
  if (c.fakeTimers) return undefined;
  return first(calls(scoped(c)).filter((x) => (/^(setTimeout|setInterval)$/.test(callee(x)) && (num(x.arguments[1]) ?? 1) > 0) || (/(^|\.)(sleep|delay)$/i.test(callee(x)) && (num(x.arguments[0]) ?? 0) > 0)));
}

function wallClock(c: Ctx) {
  if (c.fakeTimers) return undefined;
  const reads = all(scoped(c), (n): n is ts.Expression => (ts.isCallExpression(n) && /^(Date\.now|performance\.now)$/.test(callee(n))) || (ts.isNewExpression(n) && text(n.expression) === 'Date' && !n.arguments?.length));
  return first(reads);
}

function tightTimeout(c: Ctx) {
  const opts = all([c.body], ts.isPropertyAssignment).filter((p) => p.name.getText() === 'timeout' && (num(p.initializer) ?? Infinity) < 2_000);
  return first(opts);
}

const SEAMS: Array<[RegExp, string]> = [[/Math\.random/, 'random'], [/Date\.now|new Date\(\)|performance\.now/, 'clock'], [/^fetch$|globalThis\.fetch/, 'network'], [/sleep|setTimeout/i, 'real-sleep']];

/** Subject functions whose parameter defaults to a real clock, random source, fetch or sleep: name, parameter index, class. */
function seams(c: Ctx): Array<{ name: string; index: number; cls: string }> {
  const out: Array<{ name: string; index: number; cls: string }> = [];
  for (const sf of c.subjects) {
    walk(sf, (n) => {
      const fn = ts.isFunctionDeclaration(n) ? n : ts.isVariableDeclaration(n) && n.initializer && ts.isArrowFunction(n.initializer) ? n.initializer : undefined;
      const name = ts.isFunctionDeclaration(n) ? n.name?.text : ts.isVariableDeclaration(n) ? n.name.getText() : undefined;
      fn?.parameters.forEach((p, index) => {
        const hit = p.initializer && SEAMS.find(([re]) => re.test(text(p.initializer!)));
        if (hit && name) out.push({ name, index, cls: hit[1] });
      });
    });
  }
  return out;
}

const seamDefault = (cls: string) => (c: Ctx) => {
  const s = seams(c).filter((x) => x.cls === cls);
  return first(calls([c.body]).filter((x) => s.some((k) => callee(x) === k.name && x.arguments.length <= k.index)));
};

function freePort(c: Ctx) {
  const inTest = calls(scoped(c)).filter((x) => /^(free|get|find|pick)_?port$/i.test(last(x)));
  if (inTest.length) return first(inTest);
  const reserves = c.subjects.flatMap((sf) => all([sf], ts.isFunctionDeclaration).filter((f) => /\.listen\(0\b/.test(f.getText()) && /\.close\(/.test(f.getText()) && /\bport\b/.test(f.getText())));
  const used = reserves.find((f) => f.name && calls([f.getSourceFile()]).some((x) => callee(x) === f.name!.text));
  return used ? `${used.name!.text} (reserve then release, called inside the subject)` : undefined;
}

// A poll for readiness that never looks at the child it is waiting on.
function blindPoll(c: Ctx) {
  const files = [c.sf, ...c.subjects].filter((sf) => /\b(spawn|fork|execFile)\(/.test(sf.text));
  const loops = all(files, (n): n is ts.IterationStatement => ts.isWhileStatement(n) || ts.isForStatement(n) || ts.isDoStatement(n));
  const blind = loops.filter((l) => /\bfetch\(|http\.get\(|request\(/.test(l.getText()) && !/exitCode|signalCode|'exit'|"exit"|exited/.test(l.getText()));
  return first(blind);
}

const PUBLIC_URL = /^https?:\/\/(?!localhost|127\.|\[::1\]|0\.0\.0\.0)[^/\s]+\.[a-z]{2,}(\/|$)/i;
const network = (c: Ctx) => first(all([c.body], ts.isStringLiteralLike).filter((s) => PUBLIC_URL.test(s.text) && ts.isCallExpression(s.parent)));

function sharedTmp(c: Ctx) {
  const fixed = calls(scoped(c)).filter((x) => /^(join|resolve)$/.test(callee(x)) && /^tmpdir\(\)|^os\.tmpdir\(\)/.test(x.arguments[0] ? text(x.arguments[0]) : '') && x.arguments.slice(1).every((a) => ts.isStringLiteralLike(a)) && !/mkdtemp/.test(text(x.parent)));
  return first(fixed) ?? first(all(scoped(c), ts.isStringLiteralLike).filter((s) => s.text.startsWith('/tmp/')));
}

function envMutation(c: Ctx) {
  const writes = all([c.body], (n): n is ts.Expression => (ts.isBinaryExpression(n) && n.operatorToken.kind === ts.SyntaxKind.EqualsToken && /^process\.env[.[]/.test(text(n.left))) || (ts.isDeleteExpression(n) && /^process\.env[.[]/.test(text(n.expression))));
  const restores = [...all([c.body], ts.isTryStatement).filter((t) => t.finallyBlock && /process\.env/.test(t.finallyBlock.getText())), ...calls(c.file).filter((x) => /^(after|afterEach|t\.after)$/.test(callee(x)) && /process\.env/.test(x.getText()))];
  return restores.length ? undefined : first(writes);
}

const testBodies = (c: Ctx) => all([c.sf], (n): n is ts.CallExpression => ts.isCallExpression(n) && /^(test|it)$/.test(callee(n)));

// A file-level accumulator that two tests read: the second sees what the first left.
function sharedAccumulator(c: Ctx) {
  const top = c.file.filter(ts.isVariableStatement).flatMap((s) => [...s.declarationList.declarations]);
  const vars = top.filter((d) => d.initializer && /^(\[\]|new (Map|Set)\(\)|\{\})$/.test(text(d.initializer)));
  const uses = (t: ts.Node, name: string) => all([t], ts.isIdentifier).some((i) => i.text === name);
  const reset = calls(c.file).filter((x) => /^(beforeEach|afterEach)$/.test(callee(x)));
  const shared = vars.filter((v) => {
    const name = v.name.getText();
    const asserted = c.asserts.some((a) => a.actual && uses(a.actual, name));
    return asserted && testBodies(c).filter((t) => uses(t, name)).length >= 2 && !reset.some((r) => uses(r, name));
  });
  return first(shared);
}

const CLOSERS = /\b(close|closeAllConnections|kill|stop|clearInterval|clearTimeout|dispose|abort|teardown|unref|cleanup|end|rm|destroy)\(/;
function leakedHandle(c: Ctx) {
  const opened = calls([c.body]).filter((x) => /^(start([A-Z]\w*)?|spawn|fork|setInterval|watch|listen)$/.test(last(x)));
  if (!opened.length || CLOSERS.test(c.body.getText()) || calls(c.file).some((x) => /^(after|afterEach)$/.test(callee(x)) && CLOSERS.test(x.getText()))) return undefined;
  return first(opened);
}

const unordered = (c: Ctx) => first(calls([c.body]).filter((x) => callee(x) === 'Promise.race' || (/^readdir(Sync)?$/.test(last(x)) && !/\.sort\(/.test(text(x.parent)))));

// Assertions that check nothing about the system under test.
const literalAssert = (c: Ctx) => first(c.asserts.filter((a) => isLiteral(unwrap(a.actual))).map((a) => a.node));

function sameExpression(c: Ctx) {
  const same = c.asserts.filter((a) => {
    if (!a.actual || !a.expected) return false;
    const expected = ts.isIdentifier(a.expected) ? initOf(c, a.expected.text) ?? a.expected : a.expected;
    return text(unwrap(a.actual)!) === text(unwrap(expected)!) && !isLiteral(unwrap(a.actual));
  });
  return first(same.map((a) => a.node));
}

const ASSERTISH = /^(assert|expect|t\.assert)|\.(rejects|resolves)$|^(check|verify|expect|assert)\w*$/i;
function noAssert(c: Ctx) {
  if (c.asserts.length || c.helperAsserts || calls([c.body]).some((x) => ASSERTISH.test(callee(x)))) return undefined;
  return c.body === c.sf ? 'no assertion in the file' : `no assertion: ${loc(c.body)}`;
}

const awaited = (n: ts.Node) => ts.isAwaitExpression(n.parent) || ts.isReturnStatement(n.parent) || (ts.isArrowFunction(n.parent) && n.parent.body === n);
function unawaited(c: Ctx) {
  const promised = calls([c.body]).filter((x) => /^assert\.(rejects|doesNotReject)$/.test(callee(x)) || (/\.(rejects|resolves)\./.test(callee(x)) && /^expect\(/.test(text(x))));
  const props = all([c.body], ts.isPropertyAccessExpression).filter((p) => /^(rejects|resolves)$/.test(p.name.text) && /^expect\(/.test(text(p.expression)) && !ts.isPropertyAccessExpression(p.parent));
  return first([...promised.filter((x) => !awaited(x)), ...props]);
}

// Loops over a filtered list or recorded calls may run zero times; a loop over fixed cases always runs.
const FILTERED = /\.filter\(|\.mock\.calls|\.calls\b/;
// Whether the step from n up to its parent p puts n where it may never run.
function mayNotRun(n: ts.Node, p: ts.Node): boolean {
  if (ts.isIfStatement(p) || ts.isCatchClause(p) || ts.isConditionalExpression(p)) return true;
  if ((ts.isForOfStatement(p) || ts.isForInStatement(p)) && n === p.statement) return FILTERED.test(text(p.expression));
  if (ts.isTryStatement(p) && n === p.tryBlock) return !!p.catchClause && !/\bthrow\b/.test(p.catchClause.getText());
  if (!ts.isCallExpression(p) || !p.arguments.includes(n as ts.Expression)) return false;
  return /\.(then|catch)$/.test(callee(p)) || (/\.(forEach|map|some|every)$/.test(callee(p)) && FILTERED.test(callee(p)));
}

function guarded(a: Assertion, body: ts.Node): boolean {
  for (let n: ts.Node = a.node; n !== body && n.parent; n = n.parent) if (mayNotRun(n, n.parent)) return true;
  return false;
}
const allGuarded = (c: Ctx) => (c.asserts.length && !c.helperAsserts && c.asserts.every((a) => guarded(a, c.body)) ? loc(c.asserts[0].node) : undefined);

const WEAK = /^(ok|toBeTruthy|toBeDefined|not\.toBeNull|not\.toBeUndefined|toBeInstanceOf|toBeTypeOf|toHaveLength)$/;
function weak(a: Assertion): boolean {
  const actual = unwrap(a.actual);
  if (a.matcher === 'ok' && actual && ts.isBinaryExpression(actual)) return /^>=$/.test(actual.operatorToken.getText()) && text(actual.right) === '0';
  if (WEAK.test(a.matcher)) return true;
  if (actual && ts.isTypeOfExpression(actual)) return true;
  if (a.matcher === 'toMatch' && a.expected && ts.isRegularExpressionLiteral(a.expected)) return /^\/[^^$]*\*\/\w*$/.test(a.expected.text);
  return !!actual && /\.length$/.test(text(actual)) && !!a.expected && /\.length$/.test(text(a.expected));
}
const weakOnly = (c: Ctx) => (c.asserts.length && !c.helperAsserts && c.asserts.every(weak) ? loc(c.asserts[0].node) : undefined);

const mockCalled = (c: Ctx) => first(c.asserts.filter((a) => {
  const u = unwrap(a.actual);
  return !!u && ts.isCallExpression(u) && c.mocks.has(callee(u));
}).map((a) => a.node));

function subjectImports(c: Ctx): string[] {
  const dir = dirname(c.sf.fileName);
  const subjects = new Set(c.subjects.map((s) => normalize(s.fileName)));
  return c.sf.statements.filter(ts.isImportDeclaration).filter((d) => subjects.has(normalize(join(dir, (d.moduleSpecifier as ts.StringLiteral).text))))
    .flatMap((d) => [...((d.importClause?.namedBindings as ts.NamedImports | undefined)?.elements ?? [])].map((e) => e.name.text));
}

function overMocked(c: Ctx) {
  const dir = dirname(c.sf.fileName);
  const subjects = new Set(c.subjects.map((s) => normalize(s.fileName)));
  const moduleMock = calls(c.file).find((x) => /^(vi|jest)\.mock$/.test(callee(x)) && ts.isStringLiteralLike(x.arguments[0]) && subjects.has(normalize(join(dir, x.arguments[0].text))));
  if (moduleMock) return loc(moduleMock);
  const made = new Set(decls(c).filter((d) => d.initializer && ts.isCallExpression(unwrap(d.initializer)!) && subjectImports(c).includes(callee(unwrap(d.initializer) as ts.CallExpression))).map((d) => d.name.getText()));
  const replaced = all([c.body], ts.isBinaryExpression).filter((b) => b.operatorToken.kind === ts.SyntaxKind.EqualsToken && ts.isPropertyAccessExpression(b.left) && made.has(text(b.left.expression)) && (ts.isArrowFunction(b.right) || ts.isFunctionExpression(b.right)));
  return first(replaced);
}

const CALL_MATCHER = /toHaveBeenCalled|toBeCalled|toHaveBeenNthCalledWith|toHaveBeenLastCalledWith|toHaveReturned/;
function interaction(c: Ctx, a: Assertion): boolean {
  if (CALL_MATCHER.test(a.matcher)) return true;
  const t = a.actual ? text(a.actual) : '';
  if (/\.mock\.calls|callCount/.test(t) || c.mocks.has(rootOf(a.actual) ?? '')) return true;
  const loop = ts.findAncestor(a.node, ts.isForOfStatement);
  return !!loop && /\.mock\.calls/.test(text(loop.expression));
}
// A mock handed straight to the unit is its output (a callback); one inside a deps object is a collaborator.
const callbacks = (c: Ctx) => new Set(calls([c.body]).flatMap((x) => x.arguments.filter(ts.isIdentifier).map((a) => a.text)));
function interactionOnly(c: Ctx) {
  const out = callbacks(c);
  const ok = c.asserts.length && c.asserts.every((a) => interaction(c, a) && !out.has(rootOf(a.actual) ?? ''));
  return ok ? loc(c.asserts[0].node) : undefined;
}

function changeDetector(c: Ctx) {
  const pushed = calls([c.body]).filter((x) => /\.push$/.test(callee(x)) && ts.isStringLiteralLike(x.arguments[0]) && ts.findAncestor(x, (n) => ts.isCallExpression(n) && /^(vi|mock)\.fn$/.test(callee(n))));
  const logs = new Set(pushed.map((x) => callee(x).replace(/\.push$/, '')));
  return first(c.asserts.filter((a) => logs.has(a.actual ? text(a.actual) : '')).map((a) => a.node));
}

function selfSet(c: Ctx) {
  const sets = all([c.body], ts.isBinaryExpression).filter((b) => b.operatorToken.kind === ts.SyntaxKind.EqualsToken && ts.isPropertyAccessExpression(b.left));
  const hit = c.asserts.find((a) => sets.some((s) => {
    const root = rootOf(s.left as ts.Expression);
    if (!root || root !== rootOf(a.actual) || s.end > a.node.pos) return false;
    return !calls([c.body]).some((x) => x.pos > s.end && x.end < a.node.pos && x.arguments.some((arg) => rootOf(arg) === root));
  }));
  return hit && loc(hit.node);
}

const EQUALITY = /^(equal|deepEqual|strictEqual|deepStrictEqual|toBe|toEqual|toStrictEqual|toMatchSnapshot|toMatchInlineSnapshot|toMatchObject)$/;
function snapshotInput(c: Ctx) {
  const hit = c.asserts.find((a) => {
    const u = unwrap(a.actual);
    const id = u && ts.isIdentifier(u) ? u.text : undefined;
    const init = id ? initOf(c, id) : undefined;
    if (!init || !EQUALITY.test(a.matcher) || !((ts.isArrayLiteralExpression(init) && init.elements.length) || (ts.isObjectLiteralExpression(init) && init.properties.length))) return false;
    const input = (x: ts.CallExpression) => x.arguments.some((arg) => text(arg) === id) || text(x.expression).startsWith(`${id}.`);
    return calls([c.body]).some((x) => x !== a.node && !ASSERTISH.test(callee(x)) && input(x));
  });
  return hit && loc(hit.node);
}

function logicInTest(c: Ctx) {
  const subj = subjectImports(c);
  const expected = decls(c).filter((d) => /^(expected|want|exp)\b/i.test(d.name.getText()) && d.initializer && !isLiteral(d.initializer));
  const computed = expected.filter((d) => {
    const init = unwrap(d.initializer)!;
    if (ts.isCallExpression(init) && subj.includes(callee(init))) return true;
    return /\.(replace|reduce|map|filter|toLowerCase|toUpperCase|join|split)\(/.test(text(init)) || all([c.body], ts.isBinaryExpression).some((b) => b.operatorToken.kind === ts.SyntaxKind.EqualsToken && text(b.left) === d.name.getText() && !!ts.findAncestor(b, ts.isIfStatement));
  });
  return first(computed);
}

export const RULES: Rule[] = [
  { id: 'real-sleep', category: 'flaky', cls: 'real-sleep', check: realSleep },
  { id: 'wall-clock', category: 'flaky', cls: 'clock', check: wallClock },
  { id: 'tight-timeout', category: 'flaky', cls: 'real-sleep', check: tightTimeout },
  { id: 'seam-default-random', category: 'flaky', cls: 'random', check: seamDefault('random') },
  { id: 'seam-default-clock', category: 'flaky', cls: 'clock', check: seamDefault('clock') },
  { id: 'seam-default-fetch', category: 'flaky', cls: 'network', check: seamDefault('network') },
  { id: 'seam-default-sleep', category: 'flaky', cls: 'real-sleep', check: seamDefault('real-sleep') },
  { id: 'free-port', category: 'flaky', cls: 'port-race', check: freePort },
  { id: 'blind-poll', category: 'flaky', cls: 'readiness-blind', check: blindPoll },
  { id: 'public-url', category: 'flaky', cls: 'network', check: network },
  { id: 'fixed-tmp-path', category: 'flaky', cls: 'shared-state', check: sharedTmp },
  { id: 'env-unrestored', category: 'flaky', cls: 'shared-state', check: envMutation },
  { id: 'shared-accumulator', category: 'flaky', cls: 'shared-state', check: sharedAccumulator },
  { id: 'unclosed-handle', category: 'flaky', cls: 'leak', check: leakedHandle },
  { id: 'unordered', category: 'flaky', cls: 'order', check: unordered },
  { id: 'literal-assert', category: 'taut', cls: 'literal', check: literalAssert },
  { id: 'same-expression', category: 'taut', cls: 'logic-in-test', check: sameExpression },
  { id: 'no-assert', category: 'taut', cls: 'no-assert', check: noAssert },
  { id: 'unawaited-assert', category: 'taut', cls: 'no-assert', check: unawaited },
  { id: 'guarded-asserts', category: 'taut', cls: 'no-assert', check: allGuarded },
  { id: 'weak-only', category: 'taut', cls: 'truthy', check: weakOnly },
  { id: 'mock-called', category: 'taut', cls: 'mock-self', check: mockCalled },
  { id: 'unit-mocked', category: 'taut', cls: 'over-mocked', check: overMocked },
  { id: 'interaction-only', category: 'taut', cls: 'interaction-only', check: interactionOnly },
  { id: 'call-order-log', category: 'taut', cls: 'change-detector', check: changeDetector },
  { id: 'self-set', category: 'taut', cls: 'self-set', check: selfSet },
  { id: 'snapshot-input', category: 'taut', cls: 'snapshot-input', check: snapshotInput },
  { id: 'logic-in-test', category: 'taut', cls: 'logic-in-test', check: logicInTest },
];
