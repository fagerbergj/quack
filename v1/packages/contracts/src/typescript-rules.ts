import { fileURLToPath } from 'node:url';
import ts from 'typescript';
import type { Finding } from './rest-rules.ts';

// A directory under the package, so module resolution climbs to the workspace's node_modules; nothing is written there.
const DIR = fileURLToPath(new URL('../.virtual/', import.meta.url));
export const at = (file: string) => `${DIR}${file}`;
const OPTIONS: ts.CompilerOptions = {
  target: ts.ScriptTarget.ES2022,
  module: ts.ModuleKind.ESNext,
  moduleResolution: ts.ModuleResolutionKind.Bundler,
  strict: true,
  noEmit: true,
  // A .d.ts is only checked when lib checking is on.
  skipLibCheck: false,
  types: ['node'],
  typeRoots: [fileURLToPath(new URL('../../../node_modules/@types', import.meta.url))],
};

const libs = new Map<string, ts.SourceFile>();

/** A program over in-memory files (keyed by absolute path); everything else comes from disk, with parsed libs shared across programs. */
export function program(virtual: Record<string, string>, roots: string[]): ts.Program {
  const host = ts.createCompilerHost(OPTIONS, true);
  const { getSourceFile, fileExists, readFile } = host;
  host.getSourceFile = (name, ...rest) => {
    if (name in virtual) return ts.createSourceFile(name, virtual[name], rest[0], true);
    if (!libs.has(name)) libs.set(name, getSourceFile.call(host, name, ...rest)!);
    return libs.get(name);
  };
  host.fileExists = (name) => name in virtual || fileExists.call(host, name);
  // The resolver skips a directory that does not exist, and DIR is never created.
  const { directoryExists } = host;
  host.directoryExists = (dir) => `${dir}/` === DIR || directoryExists!.call(host, dir);
  host.readFile = (name) => virtual[name] ?? readFile.call(host, name);
  return ts.createProgram(roots, OPTIONS, host);
}

export type Export = { name: string; type: boolean; value: boolean; params: number; node: ts.Statement };

const isExported = (n: ts.Node) => ts.canHaveModifiers(n) && !!ts.getModifiers(n)?.some((m) => m.kind === ts.SyntaxKind.ExportKeyword);

function describe(n: ts.Statement): Omit<Export, 'node'>[] {
  if (ts.isVariableStatement(n)) return n.declarationList.declarations.map((d) => ({ name: d.name.getText(), type: false, value: true, params: 0 }));
  const name = (n as unknown as ts.NamedDeclaration).name?.getText();
  if (!name) return [];
  if (ts.isInterfaceDeclaration(n) || ts.isTypeAliasDeclaration(n)) return [{ name, type: true, value: false, params: n.typeParameters?.length ?? 0 }];
  if (ts.isClassDeclaration(n)) return [{ name, type: true, value: true, params: n.typeParameters?.length ?? 0 }];
  return [{ name, type: ts.isEnumDeclaration(n), value: true, params: 0 }];
}

/** Top-level exported declarations, one entry per name per statement (a type-and-const pair gives two). */
export const exportsOf = (sf: ts.SourceFile): Export[] => sf.statements.filter(isExported).flatMap((node) => describe(node).map((d) => ({ ...d, node })));

// The probe's module paths are absolute; show them as prev. and next.
export const oneLine = (m: string | ts.DiagnosticMessageChain) => ts.flattenDiagnosticMessageText(m, ' ').replace(/import\("[^"]*\/(prev|next)"\)\./g, '$1.').replace(/\s+/g, ' ').slice(0, 300);
const find = (code: string, message: string, path: string): Finding => ({ code, message, path });
const line = (sf: ts.SourceFile, pos: number) => sf.getLineAndCharacterOfPosition(pos).line + 1;

function compileFindings(p: ts.Program, sf: ts.SourceFile): Finding[] {
  return ts.getPreEmitDiagnostics(p, sf).map((d) => find(`ts${d.code}`, oneLine(d.messageText), `line ${line(sf, d.start ?? 0)}`));
}

function importFindings(sf: ts.SourceFile): Finding[] {
  return sf.statements.filter(ts.isImportDeclaration).flatMap((n) => {
    const spec = (n.moduleSpecifier as ts.StringLiteral).text;
    return spec.startsWith('@earendil-works/') || spec.startsWith('node:') ? [] : [find('import-not-allowed', `imports "${spec}"; only @earendil-works/* packages and node: built-ins may be named`, `line ${line(sf, n.getStart())}`)];
  });
}

const hasTsdoc = (sf: ts.SourceFile, n: ts.Node) => !!ts.getLeadingCommentRanges(sf.text, n.pos)?.some((r) => sf.text.startsWith('/**', r.pos));

function containsAny(n: ts.Node): boolean {
  return n.kind === ts.SyntaxKind.AnyKeyword || !!ts.forEachChild(n, (c) => containsAny(c) || undefined);
}

const isDefault = (s: ts.Statement) => ts.isExportAssignment(s) || (isExported(s) && !!ts.getModifiers(s as ts.HasModifiers)?.some((m) => m.kind === ts.SyntaxKind.DefaultKeyword));

function exportFindings(sf: ts.SourceFile): Finding[] {
  const out = sf.statements.filter(isDefault).map((s) => find('default-export', 'default exports are not allowed; name the export', `line ${line(sf, s.getStart())}`));
  for (const e of exportsOf(sf)) {
    if (!hasTsdoc(sf, e.node)) out.push(find('missing-tsdoc', `export "${e.name}" has no TSDoc comment saying who provides it and who consumes it`, `/${e.name}`));
    if (containsAny(e.node)) out.push(find('no-any', `export "${e.name}" uses any; name the type or use unknown`, `/${e.name}`));
  }
  return out;
}

/** Compile errors of the declaration surface on its own, plus the house rules: TSDoc on every export, no any, no default export, only allowed imports. */
export async function lintTypeScript(document: unknown): Promise<Finding[]> {
  if (typeof document !== 'string') return [find('ts-invalid', 'the document must be TypeScript source text', '')];
  const file = at('contract.d.ts');
  const p = program({ [file]: document }, [file]);
  const sf = p.getSourceFile(file)!;
  return [...compileFindings(p, sf), ...importFindings(sf), ...exportFindings(sf)];
}
