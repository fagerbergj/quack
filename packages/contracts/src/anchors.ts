// The anchored text of the Contracts app API v13: the app is its authority, so state and outdated compare what this computes.
import ts from 'typescript';
import { factText } from './facts.ts';
import type { Anchor } from './reason.ts';
import type { ContractSpec } from './spec.ts';

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const sorted = (v: unknown): unknown => (Array.isArray(v) ? v.map(sorted) : isObj(v) ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, sorted(v[k])])) : v);
/** JSON with every object's keys sorted. */
export const canonical = (v: unknown) => JSON.stringify(sorted(v));

const MISSING = Symbol('missing');
const segments = (ptr: string) => ptr.split('/').slice(1).map((s) => s.replaceAll('~1', '/').replaceAll('~0', '~'));

// RFC 6901: an array index is decimal without a leading zero.
function at(doc: unknown, ptr: string): unknown {
  if (ptr !== '' && !ptr.startsWith('/')) return MISSING;
  let v = doc;
  for (const s of ptr === '' ? [] : segments(ptr)) {
    if (Array.isArray(v) ? !/^(0|[1-9]\d*)$/.test(s) || Number(s) >= v.length : !isObj(v) || !Object.hasOwn(v, s)) return MISSING;
    v = (v as Record<string, unknown>)[s];
  }
  return v;
}

const exported = (s: ts.Statement) => ts.canHaveModifiers(s) && !!ts.getModifiers(s)?.some((m) => m.kind === ts.SyntaxKind.ExportKeyword);
function namesOf(s: ts.Statement): string[] {
  if (ts.isVariableStatement(s)) return s.declarationList.declarations.map((d) => d.name.getText());
  const name = (s as { name?: ts.Node }).name;
  return name ? [name.getText()] : [];
}

// A member is named as the page draws it: its key, `[k: string]` for an index signature, `(call)` for a call or construct one.
function memberName(m: ts.TypeElement): string {
  if (ts.isIndexSignatureDeclaration(m)) return `[${m.parameters.map((p) => p.getText().replace(/\s+/g, ' ')).join(', ')}]`;
  return m.name ? m.name.getText() : '(call)';
}

const unwrap = (t: ts.TypeNode | undefined): ts.TypeNode | undefined => (t && ts.isParenthesizedTypeNode(t) ? unwrap(t.type) : t);
// Members live in a type literal, each type-literal constituent of an intersection in order, or an array of either's element.
function typeMembers(t: ts.TypeNode | undefined, deeper: boolean): readonly ts.TypeElement[] {
  const n = unwrap(t);
  if (!n) return [];
  if (ts.isTypeLiteralNode(n)) return n.members;
  if (ts.isIntersectionTypeNode(n)) return n.types.flatMap((x) => (ts.isTypeLiteralNode(unwrap(x)!) ? [...(unwrap(x) as ts.TypeLiteralNode).members] : []));
  return deeper && ts.isArrayTypeNode(n) ? typeMembers(n.elementType, false) : [];
}

function children(n: ts.Node, seg: string): ts.Node[] {
  if (ts.isInterfaceDeclaration(n)) return n.members.filter((m) => memberName(m) === seg);
  if (ts.isTypeAliasDeclaration(n)) return typeMembers(n.type, false).filter((m) => memberName(m) === seg);
  if (ts.isFunctionDeclaration(n)) return n.parameters.filter((p) => `${p.dotDotDotToken ? '...' : ''}${p.name.getText()}` === seg);
  if (ts.isEnumDeclaration(n)) return n.members.filter((m) => m.name.getText() === seg);
  if (ts.isPropertySignature(n) || ts.isParameter(n)) return typeMembers(n.type, true).filter((m) => memberName(m) === seg);
  return [];
}

/** A typescript pointer's spans, JSDoc included, joined in source order; undefined when it names no exported declaration or member. */
export function typescriptText(source: string, ptr: string): string | undefined {
  const segs = segments(ptr);
  if (!ptr.startsWith('/') || !segs[0]) return undefined;
  const sf = ts.createSourceFile('contract.d.ts', source, ts.ScriptTarget.Latest, true);
  let nodes: ts.Node[] = sf.statements.filter((s) => exported(s) && namesOf(s).includes(segs[0]!));
  for (const seg of segs.slice(1)) nodes = nodes.flatMap((n) => children(n, seg));
  return nodes.length ? nodes.map((n) => source.slice(n.getStart(sf, true), n.getEnd())).join('\n') : undefined;
}

const LISTS = { guarantee: 'guarantees', nonGoal: 'nonGoals' } as const;
const idOf = (f: unknown) => (isObj(f) && typeof f.id === 'string' ? f.id : undefined);

/** The anchor's text in one spec, or undefined when it does not resolve there. */
export function anchoredText(spec: Partial<ContractSpec>, anchor: Anchor): string | undefined {
  if (anchor.on === 'document') {
    if (typeof spec.document === 'string') return typescriptText(spec.document, anchor.ptr);
    const v = at(spec.document, anchor.ptr);
    return v === MISSING ? undefined : canonical(v);
  }
  if (anchor.on === 'choice') {
    const c = (Array.isArray(spec.choices) ? spec.choices : []).find((x) => isObj(x) && x.id === anchor.id);
    return c && canonical(c);
  }
  if (anchor.on === 'change') {
    const rows = (Array.isArray(spec.changes) ? spec.changes : []).filter((r) => isObj(r) && r.choice === anchor.choice);
    return rows.length ? canonical(rows) : undefined;
  }
  const list: unknown[] = Array.isArray(spec.intent?.[LISTS[anchor.on]]) ? spec.intent[LISTS[anchor.on]] : [];
  const entry = anchor.id === undefined ? list.find((f) => factText(f) === anchor.text) : list.find((f) => idOf(f) === anchor.id);
  return entry === undefined ? undefined : factText(entry);
}

const str = (v: unknown, max: number, min = 1): v is string => typeof v === 'string' && v.length >= min && v.length <= max;

/** The anchor a create body names, with only its own fields; a string naming what is wrong when it does not validate. */
export function anchorOf(v: unknown): Anchor | string {
  if (!isObj(v)) return 'anchor must be an object with on';
  if (v.on === 'document') return str(v.ptr, 1000, 0) ? { on: 'document', ptr: v.ptr } : 'anchor.ptr must be a JSON pointer of at most 1000 characters';
  if (v.on === 'choice') return str(v.id, 200) ? { on: 'choice', id: v.id } : 'anchor.id must be a choice id of 1 to 200 characters';
  if (v.on === 'change') return str(v.choice, 200) ? { on: 'change', choice: v.choice } : 'anchor.choice must be a choice id of 1 to 200 characters';
  if (v.on !== 'guarantee' && v.on !== 'nonGoal') return 'anchor.on must be document, choice, change, guarantee or nonGoal';
  if (!str(v.text, 1000)) return 'anchor.text must be the entry\'s text, 1 to 1000 characters';
  if (v.id !== undefined && !str(v.id, 200)) return 'anchor.id must be the entry\'s id, 1 to 200 characters';
  return { on: v.on, text: v.text, ...(v.id !== undefined && { id: v.id as string }) };
}
