// Tangles a diff adds or grows: top-level things in two or more files of one package that use each other at runtime with no import cycle.
import ts from 'typescript';
import { addTie, fileCycles, gate, importEdges, moveEdges, pkg, resolver, type Base, type Edge, type Graph } from './cycles.ts';

type Ref = { name: string; member?: string; line: number };
type Facts = {
  /** Each runtime top-level thing, by name, with the names it uses. */
  things: Map<string, Ref[]>;
  /** Runtime imports by local name; `*` for a namespace. */
  imports: Map<string, { spec: string; name: string }>;
  /** Exported names that are not the local name: `export { a as b }`, `export default a`. */
  aliases: Map<string, string>;
  /** `export { from as name } from spec`; `*` for `export * from`. */
  reexports: Array<{ spec: string; from: string; name: string }>;
};

const cache = new Map<string, { text: string; facts: Facts }>();
const lineOf = (sf: ts.SourceFile, n: ts.Node) => sf.getLineAndCharacterOfPosition(n.getStart(sf)).line + 1;
const isStatic = (s: ts.Statement) => ts.isInterfaceDeclaration(s) || ts.isTypeAliasDeclaration(s);

function importsOf(s: ts.ImportDeclaration, into: Facts['imports']) {
  const c = s.importClause;
  if (!c || c.isTypeOnly || !ts.isStringLiteral(s.moduleSpecifier)) return;
  const spec = s.moduleSpecifier.text;
  if (c.name) into.set(c.name.text, { spec, name: 'default' });
  const b = c.namedBindings;
  if (b && ts.isNamespaceImport(b)) into.set(b.name.text, { spec, name: '*' });
  else for (const e of b?.elements ?? []) if (!e.isTypeOnly) into.set(e.name.text, { spec, name: (e.propertyName ?? e.name).text });
}

function exportsOf(s: ts.Statement, f: Facts) {
  if (ts.isExportAssignment(s) && ts.isIdentifier(s.expression)) f.aliases.set('default', s.expression.text);
  if (!ts.isExportDeclaration(s) || s.isTypeOnly) return;
  const spec = s.moduleSpecifier && ts.isStringLiteral(s.moduleSpecifier) ? s.moduleSpecifier.text : undefined;
  const named = s.exportClause && ts.isNamedExports(s.exportClause) ? s.exportClause.elements.filter((e) => !e.isTypeOnly) : [];
  if (spec && !s.exportClause) f.reexports.push({ spec, from: '*', name: '*' });
  for (const e of named) {
    const from = (e.propertyName ?? e.name).text;
    if (spec) f.reexports.push({ spec, from, name: e.name.text });
    else f.aliases.set(e.name.text, from);
  }
}

/** The runtime things a statement declares, by name. */
function declared(s: ts.Statement): Array<[string, ts.Node]> {
  if (ts.isFunctionDeclaration(s) || ts.isClassDeclaration(s)) return [[s.name?.text ?? 'default', s]];
  if (ts.isEnumDeclaration(s)) return [[s.name.text, s]];
  if (!ts.isVariableStatement(s)) return [];
  return s.declarationList.declarations.flatMap((d) => (ts.isIdentifier(d.name) ? [[d.name.text, d] as [string, ts.Node]] : []));
}

/** Whether `node` names something rather than declaring it or naming a member. */
function isUse(node: ts.Identifier, parent: ts.Node): boolean {
  const p = parent as ts.Node & { name?: ts.Node; propertyName?: ts.Node };
  return (p.name !== node || ts.isShorthandPropertyAssignment(parent)) && p.propertyName !== node;
}

/** The names a binding binds, through destructuring. */
const bound = (n: ts.BindingName): string[] => (ts.isIdentifier(n) ? [n.text] : n.elements.flatMap((e) => (ts.isOmittedExpression(e) ? [] : bound(e.name))));

const declaredIn = (statements: readonly ts.Statement[]) => statements.flatMap((s) => {
  if (ts.isVariableStatement(s)) return s.declarationList.declarations.flatMap((d) => bound(d.name));
  return (ts.isFunctionDeclaration(s) || ts.isClassDeclaration(s) || ts.isEnumDeclaration(s)) && s.name ? [s.name.text] : [];
});

const loopNames = (init: ts.ForInitializer | undefined) => (init && ts.isVariableDeclarationList(init) ? init.declarations.flatMap((d) => bound(d.name)) : []);

// shortcut: `var` binds in its block instead of hoisting to its function; the repo has no `var`, so hoist it if one appears.
/** The names `node` binds in its own scope: parameters and a function expression's name, a block's declarations, a loop's or catch's variables. */
function scopeNames(node: ts.Node): string[] {
  if (ts.isFunctionLike(node)) return [...node.parameters.flatMap((p) => bound(p.name)), ...(ts.isFunctionExpression(node) && node.name ? [node.name.text] : [])];
  if (ts.isBlock(node) || ts.isModuleBlock(node)) return declaredIn(node.statements);
  if (ts.isCaseBlock(node)) return declaredIn(node.clauses.flatMap((c) => [...c.statements]));
  if (ts.isForStatement(node) || ts.isForInStatement(node) || ts.isForOfStatement(node)) return loopNames(node.initializer);
  return ts.isCatchClause(node) && node.variableDeclaration ? bound(node.variableDeclaration.name) : [];
}

/** The names `root` uses outside type positions, of those in `wanted`, unless a scope inside it rebinds them. */
function usesIn(sf: ts.SourceFile, root: ts.Node, wanted: Set<string>, namespaces: Set<string>): Ref[] {
  const refs: Ref[] = [];
  const visit = (node: ts.Node, parent: ts.Node, shadowed: Set<string>): void => {
    if (ts.isTypeNode(node) && !ts.isExpressionWithTypeArguments(node)) return;
    if (ts.isHeritageClause(node) && node.token === ts.SyntaxKind.ImplementsKeyword) return;
    const inner = scopeNames(node).filter((n) => wanted.has(n));
    const hidden = inner.length ? new Set([...shadowed, ...inner]) : shadowed;
    if (ts.isPropertyAccessExpression(node) && ts.isIdentifier(node.expression) && namespaces.has(node.expression.text) && !hidden.has(node.expression.text)) {
      refs.push({ name: node.expression.text, member: node.name.text, line: lineOf(sf, node) });
      return;
    }
    if (ts.isIdentifier(node) && wanted.has(node.text) && !hidden.has(node.text) && isUse(node, parent)) refs.push({ name: node.text, line: lineOf(sf, node) });
    ts.forEachChild(node, (child) => visit(child, node, hidden));
  };
  const own = new Set(scopeNames(root).filter((n) => wanted.has(n)));
  ts.forEachChild(root, (child) => visit(child, root, own));
  return refs;
}

function factsOf(path: string, text: string): Facts {
  const hit = cache.get(path);
  if (hit?.text === text) return hit.facts;
  const sf = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, false, path.endsWith('x') ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
  const f: Facts = { things: new Map(), imports: new Map(), aliases: new Map(), reexports: [] };
  for (const s of sf.statements) if (ts.isImportDeclaration(s)) importsOf(s, f.imports);
  else exportsOf(s, f);
  const tops = sf.statements.filter((s) => !isStatic(s)).flatMap(declared);
  const wanted = new Set([...tops.map(([n]) => n), ...f.imports.keys()]);
  const namespaces = new Set([...f.imports].filter(([, i]) => i.name === '*').map(([n]) => n));
  for (const [name, node] of tops) f.things.set(name, [...(f.things.get(name) ?? []), ...usesIn(sf, node, wanted, namespaces)]);
  cache.set(path, { text, facts: f });
  return f;
}

type Use = Edge & { user: string; used: string };

/** Each runtime use of one top-level thing by another, both in one package, past barrels and aliases. */
function thingUses(sources: Map<string, string>): Use[] {
  const resolve = resolver(sources);
  const facts = (f: string) => (sources.has(f) ? factsOf(f, sources.get(f)!) : undefined);
  const declOf = (file: string | undefined, name: string, depth = 0): [string, string] | undefined => {
    const f = file ? facts(file) : undefined;
    if (!f || depth > 8) return undefined;
    const local = f.aliases.get(name) ?? name;
    if (f.things.has(local)) return [file!, local];
    const imported = f.imports.get(local);
    if (imported && imported.name !== '*') return declOf(resolve(file!, imported.spec), imported.name, depth + 1);
    // The first re-export that holds the name wins; TS drops a name two `export *` both provide, so no import resolves there.
    for (const r of f.reexports) if (r.name === name || r.name === '*') {
      const hit = declOf(resolve(file!, r.spec), r.name === '*' ? name : r.from, depth + 1);
      if (hit) return hit;
    }
    return undefined;
  };
  const uses: Use[] = [];
  for (const file of sources.keys()) {
    const f = pkg(file) ? facts(file)! : undefined;
    for (const [user, refs] of f?.things ?? []) for (const r of refs) {
      const i = f!.imports.get(r.name);
      const used = f!.things.has(r.name) ? [file, r.name] : i && declOf(resolve(file, i.spec), i.name === '*' ? r.member ?? '' : i.name);
      if (!used || (used[0] === file && used[1] === user) || pkg(used[0]) !== pkg(file)) continue;
      uses.push({ from: file, to: used[0], line: r.line, user, used: used[1] });
    }
  }
  return uses;
}

function graphOf(uses: Use[]): Graph {
  const g: Graph = new Map();
  for (const u of uses) addTie(g, pkg(u.from)!, `${u.from}#${u.user}`, `${u.to}#${u.used}`, { ...u, step: `${u.from}:${u.line} ${u.user} → ${u.used}` });
  return g;
}

/** Tangles in `head` across two or more files that no single import cycle holds, with a use `base` lacks. */
export function tangleFindings(base: Base, head: Map<string, string>) {
  const cycles = fileCycles(importEdges(head));
  return gate(graphOf(moveEdges(thingUses(base.sources), base.moved)), graphOf(thingUses(head)), (_, members) => {
    const files = [...new Set(members.map((m) => m.slice(0, m.indexOf('#'))))];
    const inOneCycle = files.every((f) => cycles.has(f) && cycles.get(f) === cycles.get(files[0]));
    return files.length > 1 && !inOneCycle ? `tangle of ${members.length} things across ${files.length} files` : undefined;
  });
}
