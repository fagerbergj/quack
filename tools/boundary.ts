import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join, relative, resolve, sep } from 'node:path';
import ts from 'typescript';

/** Package layers (RFC extension dependencies): arrows point down only; services and tools sit beside the stack as leaves. */
export const LAYERS: Record<string, 'core' | 'foundation' | 'feature' | 'leaf'> = {
  sdk: 'core', session: 'core', catalog: 'core', distributor: 'core', runtime: 'core', web: 'core',
  judge: 'foundation', workflows: 'foundation', slots: 'foundation', traces: 'foundation', subagent: 'foundation',
  github: 'feature', home: 'feature', sleeper: 'feature', remarkable: 'feature', research: 'feature', contracts: 'feature',
  schedules: 'leaf', eval: 'leaf', cli: 'leaf',
};

/** The one core package an extension may import: the sdk is the extension contract. */
const CONTRACT = 'sdk';

/** Known violations to retire, as 'from -> to': why. A stale entry fails the test, so the list only shrinks. */
export const ALLOWED: Record<string, string> = {
  'workflows -> session': 'type-only import of Dispatch, DispatchingExtension and Resume; moving them into @quack/sdk changes the SDK surface',
};

const CAN_IMPORT = { core: ['core'], foundation: ['foundation'], feature: ['foundation'], leaf: ['core', 'foundation'] } as const;

/** Why `from` may not import `to`, or undefined. Runtime assembles every package; only core and leaves may import it back. */
export function arrowProblem(from: string, to: string): string | undefined {
  if (from === to || from === 'runtime') return undefined;
  const [a, b] = [LAYERS[from], LAYERS[to]];
  if (!a || !b) return `${!a ? from : to} has no layer in tools/boundary.ts`;
  if (to === 'runtime' ? a === 'core' || a === 'leaf' : to === CONTRACT || (CAN_IMPORT[a] as readonly string[]).includes(b)) return undefined;
  return `${from} (${a}) must not import ${to} (${b})${a === 'feature' || a === 'foundation' ? '; extensions reach core through @quack/sdk' : ''}`;
}

type Edge = { from: string; to: string; via: string; declared: boolean };

/** Every module specifier a file names: static, side-effect, re-export, dynamic and type imports, and require(). */
export function specifiers(path: string, text: string): string[] {
  const sf = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true);
  const out: string[] = [];
  const literal = (n: ts.Node | undefined) => n && (ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n)) && out.push(n.text);
  const visit = (node: ts.Node): void => {
    if (ts.isImportDeclaration(node) || ts.isExportDeclaration(node)) literal(node.moduleSpecifier);
    else if (ts.isImportEqualsDeclaration(node) && ts.isExternalModuleReference(node.moduleReference)) literal(node.moduleReference.expression);
    else if (ts.isImportTypeNode(node) && ts.isLiteralTypeNode(node.argument)) literal(node.argument.literal);
    else if (ts.isCallExpression(node) && (node.expression.kind === ts.SyntaxKind.ImportKeyword || (ts.isIdentifier(node.expression) && node.expression.text === 'require'))) {
      const [arg] = node.arguments;
      if (arg && !ts.isStringLiteral(arg) && !ts.isNoSubstitutionTemplateLiteral(arg)) out.push(`<computed ${arg.getText(sf)}>`);
      literal(arg);
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
  return out;
}

/** The workspace package a specifier reaches from `file` under `packagesDir`, if any; a computed one cannot be checked. */
function target(packagesDir: string, file: string, spec: string): string | undefined {
  if (spec.startsWith('@quack/')) return spec.slice(7).split('/')[0];
  if (spec.startsWith('<computed')) return spec;
  if (!spec.startsWith('.')) return undefined;
  const rel = relative(packagesDir, resolve(dirname(file), spec));
  return rel.startsWith('..') ? '<outside packages>' : rel.split(sep)[0];
}

/** Each package's imports of other workspace packages from src (test files excluded), plus its declared runtime dependencies. */
export function packageEdges(packagesDir: string): Edge[] {
  const edges: Edge[] = [];
  for (const dir of readdirSync(packagesDir)) {
    const pkg = JSON.parse(readFileSync(join(packagesDir, dir, 'package.json'), 'utf8'));
    const deps = Object.keys({ ...pkg.dependencies, ...pkg.peerDependencies }).filter((n) => n.startsWith('@quack/')).map((n) => n.slice(7));
    for (const to of deps) edges.push({ from: dir, to, via: 'package.json', declared: true });
    const src = join(packagesDir, dir, 'src');
    for (const file of readdirSync(src, { recursive: true, encoding: 'utf8' }).filter((f) => /\.tsx?$/.test(f))) {
      for (const spec of specifiers(file, readFileSync(join(src, file), 'utf8'))) {
        const to = target(packagesDir, join(src, file), spec);
        if (to && to !== dir) edges.push({ from: dir, to, via: `src/${file}`, declared: deps.includes(to) });
      }
    }
  }
  return edges;
}

function edgeProblem(e: Edge): string | undefined {
  // The runtime loads the deployment's extensions by computed specifier; that is its job, as assembling every package is.
  if (e.to.startsWith('<computed')) return e.from === 'runtime' ? undefined : `${e.from} imports ${e.to}, which the boundary cannot check`;
  if (e.to === '<outside packages>') return `${e.from} imports a file outside packages/`;
  // Workspaces hoist every package, so an undeclared import resolves here and breaks only where the package ships alone.
  return arrowProblem(e.from, e.to) ?? (e.declared ? undefined : `${e.from} imports @quack/${e.to} without declaring it in package.json`);
}

/** Boundary problems, minus allow-listed arrows; allow-list entries that no longer occur are problems too. */
export function boundaryProblems(edges: Edge[], allowed = ALLOWED): string[] {
  const seen = new Set<string>();
  const problems = edges.flatMap((e) => {
    const why = edgeProblem(e);
    if (!why) return [];
    seen.add(`${e.from} -> ${e.to}`);
    return `${e.from} -> ${e.to}` in allowed ? [] : [`${why} (${e.via})`];
  });
  return [...new Set(problems), ...Object.keys(allowed).filter((k) => !seen.has(k)).map((k) => `allow-list entry ${k} (${allowed[k]}) is no longer a violation; remove it`)];
}
