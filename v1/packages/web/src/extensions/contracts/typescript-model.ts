import ts from 'typescript';
import type { Decl, Field, Model } from './spec.ts';

const flat = (n: ts.Node) => n.getText().replace(/\s+/g, ' ');
const docOf = (n: ts.Node) => ts.getJSDocCommentsAndTags(n).filter(ts.isJSDoc).map((d) => ts.getTextOfJSDocComment(d.comment) ?? '').filter(Boolean).join('\n\n') || undefined;
// TypeScript attaches a doc comment only when it starts a line, so `{ a: string; /** b's doc */ b: string }` is read from the text before b.
const inlineDoc = (n: ts.Node) => n.getSourceFile().text.slice(n.pos, n.getStart()).match(/\/\*\*([\s\S]*?)\*\/\s*$/)?.[1]!.replace(/^\s*\* ?/gm, '').trim() || undefined;
const row = (name: string, type: string, required: boolean, depth: number, ptr: string, desc?: string): Field => ({ name, type, required, desc, depth, ptr, facts: [] });

// An inline object (or an array of one) is listed field by field under its parent row.
function nested(t: ts.TypeNode | undefined, at: string, depth: number): { type?: string; rows: Field[] } {
  if (t && ts.isTypeLiteralNode(t)) return { type: 'object', rows: members(t.members, at, depth) };
  if (t && ts.isArrayTypeNode(t) && ts.isTypeLiteralNode(t.elementType)) return { type: 'object[]', rows: members(t.elementType.members, at, depth) };
  return { rows: [] };
}

const params = (ps: ts.NodeArray<ts.ParameterDeclaration>) => ps.map((p) => p.getText().replace(/\s+/g, ' ')).join(', ');

function memberName(m: ts.TypeElement) {
  if (m.name) return m.name.getText();
  return ts.isIndexSignatureDeclaration(m) ? `[${params(m.parameters)}]` : '(call)';
}

function members(list: ts.NodeArray<ts.TypeElement>, at: string, depth: number): Field[] {
  return list.flatMap((m) => {
    const name = memberName(m), own = `${at}/${name}`;
    const sig = ts.isMethodSignature(m) || ts.isCallSignatureDeclaration(m);
    const inner = sig ? { rows: [] } : nested((m as ts.PropertySignature).type, own, depth + 1);
    const type = sig ? `(${params(m.parameters)}) => ${m.type ? flat(m.type) : 'void'}` : (inner.type ?? ((m as ts.PropertySignature).type ? flat((m as ts.PropertySignature).type!) : 'unknown'));
    return [row(name, type, !m.questionToken, depth, own, docOf(m) ?? inlineDoc(m)), ...inner.rows];
  });
}

const hasObject = (t: ts.TypeNode) => ts.isTypeLiteralNode(t) || (ts.isIntersectionTypeNode(t) && t.types.some(ts.isTypeLiteralNode));

// A variant is named by its first literal-typed field (`kind: 'sqlite'`), the way a discriminated union is read.
function variantName(v: ts.TypeNode, i: number) {
  const parts: readonly ts.TypeNode[] = ts.isIntersectionTypeNode(v) ? v.types : [v];
  const lit = parts.filter(ts.isTypeLiteralNode).flatMap((x) => [...x.members]).find((m) => ts.isPropertySignature(m) && m.type && ts.isLiteralTypeNode(m.type));
  return lit ? `${lit.name!.getText()}: ${flat((lit as ts.PropertySignature).type!)}` : `variant ${i + 1}`;
}

// A type alias lists an object's fields, a union with objects as one row per variant, and an intersection's object parts beside the rest.
function body(t: ts.TypeNode, at: string, depth = 0): Pick<Decl, 'rows' | 'text'> {
  if (ts.isTypeLiteralNode(t)) return { rows: members(t.members, at, depth) };
  if (ts.isUnionTypeNode(t) && t.types.some(hasObject)) {
    return {
      rows: t.types.flatMap((v, i) => {
        const b = body(v, `${at}/${i}`, depth + 1);
        return [{ ...row(variantName(v, i), b.rows.length ? (b.text ?? 'object') : (b.text ?? ''), false, depth, `${at}/${i}`), where: 'one of' }, ...b.rows];
      }),
    };
  }
  if (ts.isIntersectionTypeNode(t) && t.types.some(ts.isTypeLiteralNode)) {
    const rest = t.types.filter((x) => !ts.isTypeLiteralNode(x)).map(flat);
    return { rows: t.types.filter(ts.isTypeLiteralNode).flatMap((x) => members(x.members, at, depth)), text: rest.length ? `${rest.join(' & ')} & …` : undefined };
  }
  return { rows: [], text: flat(t) };
}

function fn(d: ts.FunctionDeclaration, at: string): Pick<Decl, 'rows' | 'text'> {
  const rows = d.parameters.flatMap((p) => {
    const name = `${p.dotDotDotToken ? '...' : ''}${p.name.getText()}`, own = `${at}/${name}`;
    const inner = nested(p.type, own, 1);
    const desc = ts.getJSDocParameterTags(p).map((t) => ts.getTextOfJSDocComment(t.comment)).find(Boolean);
    return [row(name, inner.type ?? (p.type ? flat(p.type) : 'unknown'), !p.questionToken && !p.initializer && !p.dotDotDotToken, 0, own, desc), ...inner.rows];
  });
  const ret = nested(d.type, `${at}/returns`, 1);
  const returns = d.type ? [row('returns', ret.type ?? flat(d.type), false, 0, `${at}/returns`), ...ret.rows] : [];
  return { rows: [...rows, ...returns] };
}

const generics = (n: { typeParameters?: ts.NodeArray<ts.TypeParameterDeclaration> }) => (n.typeParameters?.length ? `<${n.typeParameters.map(flat).join(', ')}>` : undefined);
const isExported = (s: ts.Statement) => !!ts.getModifiers(s as ts.HasModifiers)?.some((m) => m.kind === ts.SyntaxKind.ExportKeyword);

function declsOf(s: ts.Statement): Array<Omit<Decl, 'uses' | 'exported'> & { node: ts.Node }> {
  if (ts.isTypeAliasDeclaration(s)) return [{ name: s.name.text, kind: 'type', generics: generics(s), doc: docOf(s), ...body(s.type, `/${s.name.text}`), node: s }];
  if (ts.isInterfaceDeclaration(s)) {
    const ext = s.heritageClauses?.map(flat).join(' ');
    return [{ name: s.name.text, kind: 'interface', generics: generics(s), doc: docOf(s), rows: members(s.members, `/${s.name.text}`, 0), text: ext, node: s }];
  }
  if (ts.isFunctionDeclaration(s) && s.name) return [{ name: s.name.text, kind: 'function', generics: generics(s), doc: docOf(s), ...fn(s, `/${s.name.text}`), node: s }];
  if (ts.isVariableStatement(s)) {
    return s.declarationList.declarations.map((d) => ({ name: d.name.getText(), kind: 'const' as const, doc: docOf(d), ...(d.type ? body(d.type, `/${d.name.getText()}`) : { rows: [] }), node: d }));
  }
  if (ts.isEnumDeclaration(s)) {
    const rows = s.members.map((m) => row(m.name.getText(), m.initializer ? flat(m.initializer) : '', false, 0, `/${s.name.text}/${m.name.getText()}`, docOf(m) ?? inlineDoc(m)));
    return [{ name: s.name.text, kind: 'enum', doc: docOf(s), rows, node: s }];
  }
  const name = (s as unknown as ts.NamedDeclaration).name?.getText();
  return name ? [{ name, kind: 'other', doc: docOf(s), rows: [], text: flat(s), node: s }] : [];
}

function referenced(n: ts.Node, out = new Set<string>()): Set<string> {
  if (ts.isTypeReferenceNode(n) || ts.isExpressionWithTypeArguments(n)) out.add((ts.isTypeReferenceNode(n) ? n.typeName : n.expression).getText().split('.')[0]!);
  if (ts.isTypeQueryNode(n)) out.add(n.exprName.getText().split('.')[0]!);
  ts.forEachChild(n, (c) => void referenced(c, out));
  return out;
}

/** The declarations of a .d.ts in source order, with the other declarations each one names; parse only, so a file that would not compile still lists. */
export function declarations(src: string): Model {
  const sf = ts.createSourceFile('contract.d.ts', src, ts.ScriptTarget.ES2022, true);
  const imports = sf.statements.filter(ts.isImportDeclaration).map((i) => {
    const b = i.importClause?.namedBindings;
    const names = b && ts.isNamedImports(b) ? b.elements.map((e) => e.name.text) : b ? [b.name.text] : [i.importClause?.name?.text ?? ''];
    return { from: (i.moduleSpecifier as ts.StringLiteral).text, names: names.filter(Boolean) };
  });
  const found = sf.statements.flatMap((s) => declsOf(s).map((d) => ({ ...d, exported: isExported(s) })));
  const names = new Set(found.map((d) => d.name));
  const decls = found.map(({ node, ...d }) => ({ ...d, uses: [...referenced(node)].filter((n) => n !== d.name && names.has(n)) }));
  return { imports, decls };
}
