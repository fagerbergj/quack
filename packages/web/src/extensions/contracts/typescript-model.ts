import { parse } from '@babel/parser';
import type * as t from '@babel/types';
import type { Decl, Field, Model } from './spec.ts';

// Babel's parser is about a tenth of the TypeScript compiler's size; node text is sliced from the source of the one synchronous parse in flight.
let src = '';
const text = (n: t.Node) => src.slice(n.start!, n.end!);
const flat = (n: t.Node) => text(n).replace(/\s+/g, ' ');
const annotation = (a: t.Node | null | undefined): t.TSType | undefined => (a?.type === 'TSTypeAnnotation' ? a.typeAnnotation : undefined);
// A binding's name without its annotation or `?`: `ctx?: Context` is ctx, `...rest: R[]` is ...rest.
const bare = (n: t.Node) => src.slice(n.start!, ('typeAnnotation' in n && n.typeAnnotation?.start) || n.end!).trim().replace(/\?$/, '');
const typeOf = (n: t.Node) => annotation('typeAnnotation' in n ? n.typeAnnotation : undefined);

// A /** */ block's lines without their leading `*`, dedented by their common margin as the compiler does; its description runs until the first @tag.
function jsdocLines(c: t.Comment) {
  const lines = c.value.slice(1).split('\n').map((l, i) => (i ? l.replace(/^\s*\*/, '') : l));
  const margin = Math.min(...lines.filter((l) => l.trim()).map((l) => l.length - l.trimStart().length));
  return lines.map((l) => l.slice(margin).trimEnd());
}
const jsdocs = (n: t.Node) => (n.leadingComments ?? []).filter((c) => c.type === 'CommentBlock' && c.value.startsWith('*')).map(jsdocLines);
function description(lines: string[]) {
  const end = lines.findIndex((l) => l.startsWith('@'));
  return lines.slice(0, end < 0 ? undefined : end).join('\n').trim();
}
const docOf = (n: t.Node) => jsdocs(n).map(description).filter(Boolean).join('\n\n') || undefined;
// `@param {T} [name=x] text`, whose text runs on to the next tag.
function paramDoc(owner: t.Node, name: string) {
  const tags = jsdocs(owner).flatMap((lines) => lines.join('\n').split(/\n(?=@)/));
  return tags.map((tag) => /^@param\s+(?:\{[^}]*\}\s*)?\[?([\w$]+)\S*\s*([\s\S]*)$/.exec(tag)).find((m) => m?.[1] === name)?.[2]!.trim() || undefined;
}
const row = (name: string, type: string, required: boolean, depth: number, ptr: string, desc?: string): Field => ({ name, type, required, desc, depth, ptr, facts: [] });

// An inline object (or an array of one) is listed field by field under its parent row.
function nested(tn: t.TSType | undefined, at: string, depth: number): { type?: string; rows: Field[] } {
  if (tn?.type === 'TSTypeLiteral') return { type: 'object', rows: members(tn.members, at, depth) };
  if (tn?.type === 'TSArrayType' && tn.elementType.type === 'TSTypeLiteral') return { type: 'object[]', rows: members(tn.elementType.members, at, depth) };
  return { rows: [] };
}

const params = (ps: t.Node[]) => ps.map(flat).join(', ');

function memberName(m: t.TSTypeElement) {
  if (m.type === 'TSPropertySignature' || m.type === 'TSMethodSignature') return m.computed ? `[${text(m.key)}]` : text(m.key);
  return m.type === 'TSIndexSignature' ? `[${params(m.parameters)}]` : '(call)';
}

function members(list: t.TSTypeElement[], at: string, depth: number): Field[] {
  return list.flatMap((m) => {
    const name = memberName(m), own = `${at}/${name}`, of = annotation(m.typeAnnotation);
    const sig = m.type === 'TSPropertySignature' || m.type === 'TSIndexSignature' ? undefined : m.parameters;
    const inner = sig ? { rows: [] } : nested(of, own, depth + 1);
    const type = sig ? `(${params(sig)}) => ${of ? flat(of) : 'void'}` : (inner.type ?? (of ? flat(of) : 'unknown'));
    const optional = (m.type === 'TSPropertySignature' || m.type === 'TSMethodSignature') && Boolean(m.optional);
    return [row(name, type, !optional, depth, own, docOf(m)), ...inner.rows];
  });
}

const hasObject = (tn: t.TSType) => tn.type === 'TSTypeLiteral' || (tn.type === 'TSIntersectionType' && tn.types.some((x) => x.type === 'TSTypeLiteral'));

// A variant is named by its first literal-typed field (`kind: 'sqlite'`), the way a discriminated union is read.
function variantName(v: t.TSType, i: number) {
  const parts = v.type === 'TSIntersectionType' ? v.types : [v];
  const lit = parts.flatMap((x) => (x.type === 'TSTypeLiteral' ? x.members : [])).find((m) => m.type === 'TSPropertySignature' && annotation(m.typeAnnotation)?.type === 'TSLiteralType');
  return lit?.type === 'TSPropertySignature' ? `${text(lit.key)}: ${flat(annotation(lit.typeAnnotation)!)}` : `variant ${i + 1}`;
}

// A type alias lists an object's fields, a union with objects as one row per variant, and an intersection's object parts beside the rest.
function body(tn: t.TSType, at: string, depth = 0): Pick<Decl, 'rows' | 'text'> {
  if (tn.type === 'TSTypeLiteral') return { rows: members(tn.members, at, depth) };
  if (tn.type === 'TSUnionType' && tn.types.some(hasObject)) {
    return {
      rows: tn.types.flatMap((v, i) => {
        const b = body(v, `${at}/${i}`, depth + 1);
        return [{ ...row(variantName(v, i), b.rows.length ? (b.text ?? 'object') : (b.text ?? ''), false, depth, `${at}/${i}`), where: 'one of' }, ...b.rows];
      }),
    };
  }
  if (tn.type === 'TSIntersectionType' && tn.types.some((x) => x.type === 'TSTypeLiteral')) {
    const rest = tn.types.filter((x) => x.type !== 'TSTypeLiteral').map(flat);
    return { rows: tn.types.flatMap((x) => (x.type === 'TSTypeLiteral' ? members(x.members, at, depth) : [])), text: rest.length ? `${rest.join(' & ')} & …` : undefined };
  }
  return { rows: [], text: flat(tn) };
}

// A parameter's name, type and whether a caller must pass it; a default sits on its AssignmentPattern's left.
function param(p: t.Node) {
  if (p.type === 'AssignmentPattern') return { name: bare(p.left), type: typeOf(p.left), required: false };
  return { name: bare(p), type: typeOf(p), required: p.type !== 'RestElement' && !('optional' in p && p.optional) };
}

function fn(d: t.TSDeclareFunction | t.FunctionDeclaration, at: string, owner: t.Node): Pick<Decl, 'rows' | 'text'> {
  const rows = d.params.flatMap((p) => {
    const { name, type, required } = param(p), own = `${at}/${name}`;
    const inner = nested(type, own, 1);
    return [row(name, inner.type ?? (type ? flat(type) : 'unknown'), required, 0, own, paramDoc(owner, name.replace(/^\.\.\./, ''))), ...inner.rows];
  });
  const of = annotation(d.returnType);
  const ret = nested(of, `${at}/returns`, 1);
  const returns = of ? [row('returns', ret.type ?? flat(of), false, 0, `${at}/returns`), ...ret.rows] : [];
  return { rows: [...rows, ...returns] };
}

const generics = (n: { typeParameters?: t.Node | null }) => (n.typeParameters?.type === 'TSTypeParameterDeclaration' && n.typeParameters.params.length ? `<${n.typeParameters.params.map(flat).join(', ')}>` : undefined);
const isExport = (s: t.Statement) => s.type === 'ExportNamedDeclaration' || s.type === 'ExportDefaultDeclaration';
const unwrap = (s: t.Statement): t.Node => (isExport(s) && (s as t.ExportNamedDeclaration).declaration) || s;

// A member with no initializer takes the previous numeric value plus one, from 0, as the compiler numbers it.
function enumRows(e: t.TSEnumDeclaration): Field[] {
  let next = 0;
  return e.members.map((m) => {
    // From the text, so `-1` and `0x10` count as numbers; a string initializer is NaN and leaves the next member unnumbered.
    const lit = m.initializer && !Number.isNaN(Number(flat(m.initializer))) ? Number(flat(m.initializer)) : undefined;
    const value = m.initializer ? flat(m.initializer) : String(next);
    next = (lit ?? (m.initializer ? NaN : next)) + 1;
    return row(text(m.id), value, false, 0, `/${e.id.name}/${text(m.id)}`, docOf(m));
  });
}

// `s` is the statement as written, which carries the doc comment and the export; `d` the declaration inside it.
function declsOf(d: t.Node, s: t.Statement): Array<Omit<Decl, 'uses' | 'exported'> & { node: t.Node }> {
  const doc = docOf(s);
  if (d.type === 'TSTypeAliasDeclaration') return [{ name: d.id.name, kind: 'type', generics: generics(d), doc, ...body(d.typeAnnotation, `/${d.id.name}`), node: d }];
  if (d.type === 'TSInterfaceDeclaration') {
    const ext = d.extends?.length ? `extends ${d.extends.map(flat).join(', ')}` : undefined;
    return [{ name: d.id.name, kind: 'interface', generics: generics(d), doc, rows: members(d.body.body, `/${d.id.name}`, 0), text: ext, node: d }];
  }
  if ((d.type === 'TSDeclareFunction' || d.type === 'FunctionDeclaration') && d.id) return [{ name: d.id.name, kind: 'function', generics: generics(d), doc, ...fn(d, `/${d.id.name}`, s), node: d }];
  if (d.type === 'VariableDeclaration') {
    return d.declarations.map((v) => ({ name: bare(v.id), kind: 'const' as const, doc, ...(typeOf(v.id) ? body(typeOf(v.id)!, `/${bare(v.id)}`) : { rows: [] }), node: v }));
  }
  if (d.type === 'TSEnumDeclaration') return [{ name: d.id.name, kind: 'enum', doc, rows: enumRows(d), node: d }];
  const id = 'id' in d ? d.id : undefined;
  return id ? [{ name: text(id), kind: 'other', doc, rows: [], text: flat(s), node: d }] : [];
}

const root = (n: t.Node) => text(n).split('.')[0]!;
const NOT_CHILDREN = new Set(['loc', 'leadingComments', 'trailingComments', 'innerComments', 'extra']);

function referenced(n: t.Node, out = new Set<string>()): Set<string> {
  if (n.type === 'TSTypeReference') out.add(root(n.typeName));
  if (n.type === 'TSExpressionWithTypeArguments') out.add(root(n.expression));
  if (n.type === 'TSTypeQuery') out.add(root(n.exprName));
  if (n.type === 'ClassDeclaration' && n.superClass) out.add(root(n.superClass));
  for (const [k, v] of Object.entries(n)) {
    if (!NOT_CHILDREN.has(k)) for (const c of [v].flat()) if (c && typeof c.type === 'string') referenced(c as t.Node, out);
  }
  return out;
}

/** The declarations of a .d.ts in source order, with the other declarations each one names; parse only, so a file that would not compile still lists
 * unless its syntax is past recovery, which throws and leaves the view on its source. */
export function declarations(source: string): Model {
  src = source;
  const { body: statements } = parse(source, { sourceType: 'module', plugins: [['typescript', { dts: true }]], errorRecovery: true }).program;
  const imports = statements.flatMap((s) => (s.type === 'ImportDeclaration' ? [{ from: s.source.value, names: s.specifiers.map((x) => x.local.name) }] : []));
  const found = statements.flatMap((s) => declsOf(unwrap(s), s).map((d) => ({ ...d, exported: isExport(s) })));
  const names = new Set(found.map((d) => d.name));
  const decls = found.map(({ node, ...d }) => ({ ...d, uses: [...referenced(node)].filter((n) => n !== d.name && names.has(n)) }));
  return { imports, decls };
}
