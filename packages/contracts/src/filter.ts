// The contracts list's q, in the house vocabulary the catalog parses: field:value terms, AND/OR/NOT, - to negate, * and ? on text.
import { parse, type LiqeQuery } from 'liqe';
import { unescapeTerm } from '@quack/sdk/query';
import { SPEC_KINDS } from './spec.ts';

/** A q the list cannot run: the app answers it 400 with this message as the detail. */
export class QueryError extends Error {}

type Fields = { key: string; title: string; kind: string; pendingApproval: boolean };
type Test = (r: Fields) => boolean;
type Tag = LiqeQuery & { type: 'Tag' };

const ENUMS: Record<string, readonly string[]> = { kind: SPEC_KINDS, pendingApproval: ['true', 'false'] };
const FIELDS = ['key', 'title', 'kind', 'pendingApproval'];
const REGEX_SPECIAL = /[\\^$.|?*+()[\]{}]/g;

// As the catalog matches its text fields: exact without a wildcard, else an anchored glob; a quoted value is literal.
function textTest(field: 'key' | 'title', quoted: boolean, raw: string): Test {
  const { text, wildcards } = quoted ? { text: raw, wildcards: [] } : unescapeTerm(raw);
  if (!wildcards.length) return (r) => r[field] === text;
  const stars = new Set(wildcards);
  const glob = new RegExp(`^${text.split('').map((c, i) => (!stars.has(i) ? c.replace(REGEX_SPECIAL, '\\$&') : c === '*' ? '.*' : '.')).join('')}$`, 's');
  return (r) => glob.test(r[field]);
}

function enumTest(field: 'kind' | 'pendingApproval', quoted: boolean, raw: string): Test {
  const { text, wildcards } = quoted ? { text: raw, wildcards: [] } : unescapeTerm(raw);
  if (wildcards.length) throw new QueryError(`${field} takes no wildcard; * and ? apply to key and title`);
  if (!ENUMS[field]!.includes(text)) throw new QueryError(`${field} takes one of ${ENUMS[field]!.join(', ')}, not ${JSON.stringify(text)}`);
  return (r) => String(r[field]) === text;
}

function tagTest(q: string, tag: Tag): Test {
  if (tag.field.type === 'ImplicitField') throw new QueryError(`a bare term is not supported; this list's fields are ${FIELDS.join(', ')}`);
  const field = tag.field.name;
  if (tag.field.quoted || !FIELDS.includes(field)) throw new QueryError(`unknown field ${JSON.stringify(field)}; this list's fields are ${FIELDS.join(', ')}`);
  const expr = tag.expression;
  if (tag.operator.operator !== ':' || expr.type !== 'LiteralExpression') throw new QueryError(`${field} takes field:value; ranges, comparisons and regular expressions are not supported`);
  // An unquoted term keeps its source text, so its backslash escapes survive liqe's own reading.
  const raw = expr.quoted ? String(expr.value) : q.slice(expr.location.start, expr.location.end);
  return field === 'key' || field === 'title' ? textTest(field, expr.quoted, raw) : enumTest(field as 'kind' | 'pendingApproval', expr.quoted, raw);
}

function compile(q: string, node: LiqeQuery): Test {
  switch (node.type) {
    case 'EmptyExpression': return () => true;
    case 'ParenthesizedExpression': return compile(q, node.expression);
    case 'UnaryOperator': { const t = compile(q, node.operand); return (r) => !t(r); }
    case 'Tag': return tagTest(q, node);
    case 'LogicalExpression': {
      const [l, r] = [compile(q, node.left), compile(q, node.right)];
      return node.operator.operator === 'OR' ? (x) => l(x) || r(x) : (x) => l(x) && r(x);
    }
  }
}

const conjuncts = (n: LiqeQuery): LiqeQuery[] =>
  n.type === 'ParenthesizedExpression' ? conjuncts(n.expression)
    : n.type === 'LogicalExpression' && n.operator.operator === 'AND' ? [...conjuncts(n.left), ...conjuncts(n.right)] : [n];

// Whether every term under n is on key, which the catalog's own sessions q reads the same way.
function onlyKey(n: LiqeQuery): boolean {
  if (n.type === 'Tag') return n.field.type === 'Field' && n.field.name === 'key';
  if (n.type === 'UnaryOperator') return onlyKey(n.operand);
  if (n.type === 'ParenthesizedExpression') return onlyKey(n.expression);
  return n.type === 'LogicalExpression' && onlyKey(n.left) && onlyKey(n.right);
}

/** The q as a test of a record, plus the conjuncts the catalog can run itself (the key terms), as q source text; throws QueryError. */
export function parseFilter(q: string | null): { test: Test; pushed: string[] } {
  if (!q?.trim()) return { test: () => true, pushed: [] };
  let ast: LiqeQuery;
  try {
    ast = parse(q);
  } catch (err) {
    throw new QueryError(`${err instanceof Error ? err.message : String(err)}: escape a reserved character with a backslash (key:a\\:b) or quote the value`);
  }
  const test = compile(q, ast);
  return { test, pushed: conjuncts(ast).filter(onlyKey).map((n) => q.slice(n.location.start, n.location.end)) };
}
